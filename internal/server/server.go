// Package server 는 CollectorService 의 서버 구현이다.
//
// 세 가지 수집 결과(메타·메트릭·로그)를 각각 client streaming 으로 받는다.
// 배치 단위로 검증하되, 검증에 실패한 배치 때문에 스트림 전체를 끊지는 않는다.
// 수집기는 일부 대상이 실패해도 나머지는 계속 흘러야 하기 때문이다.
package server

import (
	"context"
	"io"
	"log/slog"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const serverVersion = "v0.2.0"

type Collector struct {
	collectorv1.UnimplementedCollectorServiceServer

	log   *slog.Logger
	store *store.Store
}

func New(log *slog.Logger, st *store.Store) *Collector {
	return &Collector{log: log, store: st}
}

// ══════════════ 메타데이터 ══════════════════════════════════

func (c *Collector) ReportMeta(stream collectorv1.CollectorService_ReportMetaServer) error {
	start := time.Now()
	var batches, items, rejected int64

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.log.Error("meta recv failed", "err", err, "batches_so_far", batches)
			return err
		}

		if reason := validateMeta(batch); reason != "" {
			rejected += int64(len(batch.GetResources()))
			c.log.Warn("meta batch rejected", "reason", reason, "agent_id", batch.GetAgentId())
			continue // 스트림은 유지한다
		}

		list := make([]*store.Resource, 0, len(batch.GetResources()))
		for _, r := range batch.GetResources() {
			list = append(list, toResource(r))
		}
		c.store.UpsertResources(list)

		batches++
		items += int64(len(batch.GetResources()))
	}

	// 배치가 모두 들어온 뒤에야 이름 → UID 해석이 가능하다.
	// 참조 대상이 같은 배치에 없을 수 있기 때문이다.
	resolved, unresolved := c.store.ResolveRelations()
	c.store.RecordStream("meta", batches, items, rejected)

	elapsed := time.Since(start)
	c.log.Info("meta stream closed",
		"batches", batches, "resources", items, "rejected", rejected,
		"relations_resolved", resolved, "relations_unresolved", unresolved,
		"elapsed_ms", elapsed.Milliseconds())

	return stream.SendAndClose(summary(batches, items, rejected, elapsed))
}

// ══════════════ 메트릭 ══════════════════════════════════════

func (c *Collector) ReportMetric(stream collectorv1.CollectorService_ReportMetricServer) error {
	start := time.Now()
	var batches, items, rejected int64

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.log.Error("metric recv failed", "err", err, "batches_so_far", batches)
			return err
		}

		if reason := validateMetric(batch); reason != "" {
			rejected += int64(len(batch.GetMetrics()))
			c.log.Warn("metric batch rejected", "reason", reason, "agent_id", batch.GetAgentId())
			continue
		}

		points := make([]store.MetricPoint, 0, len(batch.GetMetrics()))
		at := batch.GetCollectedAt().AsTime()
		for _, m := range batch.GetMetrics() {
			points = append(points, store.MetricPoint{
				Name:         m.GetName(),
				ResourceUID:  m.GetResourceUid(),
				ResourceName: m.GetResourceName(),
				Namespace:    m.GetNamespace(),
				Value:        m.GetValue(),
				At:           at,
			})
		}
		c.store.AppendMetrics(points)

		batches++
		items += int64(len(batch.GetMetrics()))
	}

	c.store.RecordStream("metric", batches, items, rejected)
	elapsed := time.Since(start)
	c.log.Info("metric stream closed",
		"batches", batches, "metrics", items, "rejected", rejected,
		"elapsed_ms", elapsed.Milliseconds())

	return stream.SendAndClose(summary(batches, items, rejected, elapsed))
}

// ══════════════ 로그 ════════════════════════════════════════

func (c *Collector) ReportLog(stream collectorv1.CollectorService_ReportLogServer) error {
	start := time.Now()
	var batches, items, rejected int64

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.log.Error("log recv failed", "err", err, "batches_so_far", batches)
			return err
		}

		if reason := validateLog(batch); reason != "" {
			rejected += int64(len(batch.GetEntries()))
			c.log.Warn("log batch rejected", "reason", reason, "agent_id", batch.GetAgentId())
			continue
		}

		lines := make([]store.LogLine, 0, len(batch.GetEntries()))
		for _, e := range batch.GetEntries() {
			lines = append(lines, store.LogLine{
				PodUID:    e.GetPodUid(),
				PodName:   e.GetPodName(),
				Namespace: e.GetNamespace(),
				Container: e.GetContainer(),
				Level:     e.GetLevel(),
				Message:   e.GetMessage(),
				At:        e.GetTimestamp().AsTime(),
			})
		}
		c.store.AppendLogs(lines)

		batches++
		items += int64(len(batch.GetEntries()))
	}

	c.store.RecordStream("log", batches, items, rejected)
	elapsed := time.Since(start)
	c.log.Info("log stream closed",
		"batches", batches, "entries", items, "rejected", rejected,
		"elapsed_ms", elapsed.Milliseconds())

	return stream.SendAndClose(summary(batches, items, rejected, elapsed))
}

// ══════════════ 제어 ════════════════════════════════════════

// Subscribe 는 수집 대상을 푸시하고 스트림을 유지한다.
// 에이전트를 재배포하지 않고 수집 범위를 바꾸기 위한 경로다.
func (c *Collector) Subscribe(req *collectorv1.SubscribeRequest, stream collectorv1.CollectorService_SubscribeServer) error {
	agentID := req.GetAgentId()
	if agentID == "" {
		return status.Error(codes.InvalidArgument, "agent_id is required")
	}

	for _, t := range targetsFor(req.GetCapabilities()) {
		if err := stream.Send(t); err != nil {
			return err
		}
	}
	c.log.Info("agent subscribed", "agent_id", agentID)

	<-stream.Context().Done()
	c.log.Info("agent unsubscribed", "agent_id", agentID)
	return nil
}

func (c *Collector) Health(_ context.Context, req *collectorv1.HealthRequest) (*collectorv1.HealthResponse, error) {
	if req.GetAgentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id is required")
	}
	return &collectorv1.HealthResponse{Ok: true, ServerVersion: serverVersion}, nil
}

// targetsFor 는 에이전트가 수집 가능하다고 알린 것 중 서버가 허용한 것만 준다.
// 지금은 전부 허용하되 주기를 서버가 정한다. 주기를 서버가 쥐고 있어야
// 에이전트를 건드리지 않고 수집 빈도를 바꿀 수 있다.
func targetsFor(capabilities []string) []*collectorv1.CollectTarget {
	interval := map[string]int32{
		"meta":   30,
		"metric": 15,
		"log":    20,
	}
	out := make([]*collectorv1.CollectTarget, 0, len(capabilities))
	for _, c := range capabilities {
		sec := int32(30)
		for prefix, v := range interval {
			if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
				sec = v
				break
			}
		}
		out = append(out, &collectorv1.CollectTarget{
			Target:          c,
			IntervalSeconds: sec,
			Enabled:         true,
		})
	}
	return out
}

// ══════════════ 검증 ════════════════════════════════════════

func validateMeta(b *collectorv1.MetaBatch) string {
	switch {
	case b.GetAgentId() == "":
		return "missing agent_id"
	case b.GetCluster() == "":
		return "missing cluster"
	case len(b.GetResources()) == 0:
		return "empty resources"
	case b.GetCollectedAt() == nil:
		return "missing collected_at"
	case b.GetCollectedAt().AsTime().After(time.Now().Add(time.Minute)):
		return "collected_at is in the future"
	}
	for _, r := range b.GetResources() {
		if r.GetUid() == "" || r.GetKind() == "" || r.GetName() == "" {
			return "resource missing uid, kind or name"
		}
	}
	return ""
}

func validateMetric(b *collectorv1.MetricBatch) string {
	switch {
	case b.GetAgentId() == "":
		return "missing agent_id"
	case b.GetCluster() == "":
		return "missing cluster"
	case len(b.GetMetrics()) == 0:
		return "empty metrics"
	case b.GetCollectedAt() == nil:
		return "missing collected_at"
	case b.GetCollectedAt().AsTime().After(time.Now().Add(time.Minute)):
		return "collected_at is in the future"
	}
	for _, m := range b.GetMetrics() {
		if m.GetName() == "" || m.GetResourceUid() == "" {
			return "metric missing name or resource_uid"
		}
	}
	return ""
}

func validateLog(b *collectorv1.LogBatch) string {
	switch {
	case b.GetAgentId() == "":
		return "missing agent_id"
	case b.GetCluster() == "":
		return "missing cluster"
	case len(b.GetEntries()) == 0:
		return "empty entries"
	case b.GetCollectedAt() == nil:
		return "missing collected_at"
	}
	for _, e := range b.GetEntries() {
		if e.GetPodUid() == "" || e.GetMessage() == "" {
			return "entry missing pod_uid or message"
		}
	}
	return ""
}

// ══════════════ 변환 ════════════════════════════════════════

func toResource(r *collectorv1.ResourceMeta) *store.Resource {
	rels := make([]store.Relation, 0, len(r.GetRelations()))
	for _, rel := range r.GetRelations() {
		rels = append(rels, store.Relation{
			Type:            rel.GetType(),
			TargetKind:      rel.GetTargetKind(),
			TargetName:      rel.GetTargetName(),
			TargetNamespace: rel.GetTargetNamespace(),
			TargetUID:       rel.GetTargetUid(),
			Resolved:        rel.GetTargetUid() != "",
		})
	}
	var created time.Time
	if r.GetCreatedAt() != nil {
		created = r.GetCreatedAt().AsTime()
	}
	return &store.Resource{
		UID:        r.GetUid(),
		Kind:       r.GetKind(),
		Name:       r.GetName(),
		Namespace:  r.GetNamespace(),
		Status:     r.GetStatus(),
		Labels:     r.GetLabels(),
		Attributes: r.GetAttributes(),
		Relations:  rels,
		CreatedAt:  created,
	}
}

func summary(batches, items, rejected int64, elapsed time.Duration) *collectorv1.ReportSummary {
	return &collectorv1.ReportSummary{
		BatchCount: batches,
		ItemCount:  items,
		Rejected:   rejected,
		ElapsedMs:  elapsed.Milliseconds(),
	}
}
