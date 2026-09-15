package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-metric-collector/gen/collector/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const serverVersion = "0.1.0"

// Collector 는 CollectorService 를 구현한다.
type Collector struct {
	collectorv1.UnimplementedCollectorServiceServer

	log *slog.Logger

	mu      sync.RWMutex
	targets map[string][]*collectorv1.CollectTarget // agent_id -> 수집 대상
	stats   Stats
}

// Stats 는 서버가 누적한 수신 통계다.
type Stats struct {
	Batches  int64
	Metrics  int64
	Rejected int64
}

func New(log *slog.Logger) *Collector {
	return &Collector{
		log:     log,
		targets: make(map[string][]*collectorv1.CollectTarget),
	}
}

// Health 는 에이전트 기동 시 연결 확인용 unary RPC 다.
func (c *Collector) Health(_ context.Context, req *collectorv1.HealthRequest) (*collectorv1.HealthResponse, error) {
	if req.GetAgentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id is required")
	}
	return &collectorv1.HealthResponse{Ok: true, ServerVersion: serverVersion}, nil
}

// Report 는 client streaming 이다.
//
// 에이전트가 배치를 연속 전송하고, 스트림이 끝나면 집계 결과를 한 번 돌려준다.
// 배치 단위로 검증하되, 검증 실패한 배치 때문에 스트림 전체를 끊지는 않는다.
// 수집기는 일부 대상이 실패해도 나머지는 계속 흘러야 하기 때문이다.
func (c *Collector) Report(stream collectorv1.CollectorService_ReportServer) error {
	start := time.Now()
	var batches, metrics, rejected int64

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.log.Error("recv failed", "err", err, "batches_so_far", batches)
			return err
		}

		if reason := validate(batch); reason != "" {
			rejected += int64(len(batch.GetMetrics()))
			c.log.Warn("batch rejected", "reason", reason, "agent_id", batch.GetAgentId())
			continue
		}

		batches++
		metrics += int64(len(batch.GetMetrics()))
	}

	c.addStats(batches, metrics, rejected)

	elapsed := time.Since(start)
	c.log.Info("report completed",
		"batches", batches, "metrics", metrics,
		"rejected", rejected, "elapsed_ms", elapsed.Milliseconds())

	return stream.SendAndClose(&collectorv1.ReportSummary{
		BatchCount:  batches,
		MetricCount: metrics,
		Rejected:    rejected,
		ElapsedMs:   elapsed.Milliseconds(),
	})
}

// Subscribe 는 server streaming 이다.
//
// 에이전트가 구독하면 현재 수집 대상을 즉시 내려주고,
// 이후 변경이 생기면 계속 푸시한다. 여기서는 데모를 위해
// 기본 대상 목록을 한 번 내려주고 컨텍스트 종료까지 대기한다.
func (c *Collector) Subscribe(req *collectorv1.SubscribeRequest, stream collectorv1.CollectorService_SubscribeServer) error {
	agentID := req.GetAgentId()
	if agentID == "" {
		return status.Error(codes.InvalidArgument, "agent_id is required")
	}

	targets := c.targetsFor(agentID, req.GetCapabilities())
	for _, t := range targets {
		if err := stream.Send(t); err != nil {
			return err
		}
	}
	c.log.Info("agent subscribed", "agent_id", agentID, "targets", len(targets))

	<-stream.Context().Done()
	c.log.Info("agent unsubscribed", "agent_id", agentID)
	return nil
}

// Snapshot 은 현재까지 누적된 수신 통계를 반환한다. (테스트·모니터링용)
func (c *Collector) Snapshot() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats
}

func (c *Collector) addStats(batches, metrics, rejected int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Batches += batches
	c.stats.Metrics += metrics
	c.stats.Rejected += rejected
}

func (c *Collector) targetsFor(agentID string, capabilities []string) []*collectorv1.CollectTarget {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.targets[agentID]; ok {
		return existing
	}

	targets := make([]*collectorv1.CollectTarget, 0, len(capabilities))
	for _, capability := range capabilities {
		targets = append(targets, &collectorv1.CollectTarget{
			Target:          capability,
			IntervalSeconds: 30,
			Enabled:         true,
		})
	}
	c.targets[agentID] = targets
	return targets
}

// validate 는 배치를 버릴 사유를 돌려준다. 빈 문자열이면 정상이다.
func validate(b *collectorv1.MetricBatch) string {
	switch {
	case b.GetAgentId() == "":
		return "missing agent_id"
	case b.GetTarget() == "":
		return "missing target"
	case len(b.GetMetrics()) == 0:
		return "empty metrics"
	case b.GetCollectedAt() == nil:
		return "missing collected_at"
	case b.GetCollectedAt().AsTime().After(time.Now().Add(time.Minute)):
		return "collected_at is in the future"
	}
	for _, m := range b.GetMetrics() {
		if m.GetName() == "" || m.GetResourceId() == "" {
			return "metric missing name or resource_id"
		}
	}
	return ""
}
