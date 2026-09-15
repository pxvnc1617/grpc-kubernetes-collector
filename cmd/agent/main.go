package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-metric-collector/gen/collector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	addr := flag.String("addr", "localhost:50051", "server address")
	agentID := flag.String("agent-id", "agent-01", "agent identifier")
	batches := flag.Int("batches", 20, "number of batches to send")
	perBatch := flag.Int("per-batch", 100, "metrics per batch")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Error("dial failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := collectorv1.NewCollectorServiceClient(conn)

	// 에이전트 전체 수명을 관리하는 컨텍스트.
	// 구독 스트림은 여기에 묶어 오래 유지하고, 개별 RPC 는 아래에서
	// 각자 짧은 타임아웃을 따로 건다. 둘을 같은 데드라인으로 묶으면
	// 장시간 구독 때문에 전송이 같이 죽는다.
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1) 연결 확인 (unary)
	if err := healthCheck(rootCtx, client, *agentID, log); err != nil {
		log.Error("health check failed", "err", err)
		os.Exit(1)
	}

	// 2) 수집 대상 구독 (server streaming)
	//    스트림은 서버가 변경을 푸시하기 위해 계속 열려 있으므로
	//    백그라운드에서 돌리고, 초기 목록만 짧게 기다렸다 진행한다.
	sub := newSubscription(log)
	go sub.run(rootCtx, client, *agentID)

	targets := sub.waitInitial(2 * time.Second)
	log.Info("targets ready", "targets", targets)

	// 3) 메트릭 배치 전송 (client streaming)
	reportCtx, reportCancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer reportCancel()

	summary, err := report(reportCtx, client, *agentID, targets, *batches, *perBatch)
	if err != nil {
		log.Error("report failed", "err", err)
		os.Exit(1)
	}

	log.Info("report summary",
		"batches", summary.GetBatchCount(),
		"metrics", summary.GetMetricCount(),
		"rejected", summary.GetRejected(),
		"elapsed_ms", summary.GetElapsedMs())
}

func healthCheck(
	ctx context.Context,
	c collectorv1.CollectorServiceClient,
	agentID string,
	log *slog.Logger,
) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res, err := c.Health(ctx, &collectorv1.HealthRequest{AgentId: agentID})
	if err != nil {
		return err
	}
	log.Info("connected", "server_version", res.GetServerVersion())
	return nil
}

// subscription 은 서버가 푸시하는 수집 대상을 계속 반영한다.
type subscription struct {
	log *slog.Logger

	mu      sync.RWMutex
	targets map[string]bool // target -> enabled

	ready chan struct{} // 초기 목록을 한 번이라도 받으면 닫힌다
	once  sync.Once
}

func newSubscription(log *slog.Logger) *subscription {
	return &subscription{
		log:     log,
		targets: make(map[string]bool),
		ready:   make(chan struct{}),
	}
}

// run 은 컨텍스트가 끝날 때까지 스트림을 유지하며 대상 변경을 반영한다.
func (s *subscription) run(ctx context.Context, c collectorv1.CollectorServiceClient, agentID string) {
	stream, err := c.Subscribe(ctx, &collectorv1.SubscribeRequest{
		AgentId:      agentID,
		Capabilities: []string{"aws/ec2", "k8s/pod", "vsphere/vm"},
	})
	if err != nil {
		s.log.Error("subscribe failed", "err", err)
		s.markReady()
		return
	}

	// 첫 배치를 받으면 waitInitial 을 풀어준다.
	// 이후에도 스트림은 유지되어 서버의 대상 변경을 계속 받는다.
	first := true
	for {
		t, err := stream.Recv()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("subscription closed", "err", err)
			}
			s.markReady()
			return
		}

		s.mu.Lock()
		s.targets[t.GetTarget()] = t.GetEnabled()
		s.mu.Unlock()

		s.log.Info("target updated",
			"target", t.GetTarget(),
			"enabled", t.GetEnabled(),
			"interval_seconds", t.GetIntervalSeconds())

		if first {
			// 서버가 초기 목록을 연달아 보내므로 잠깐 더 받아본 뒤 풀어준다.
			first = false
			go func() {
				time.Sleep(200 * time.Millisecond)
				s.markReady()
			}()
		}
	}
}

// waitInitial 은 초기 대상 목록을 기다린다. 타임아웃이면 현재까지 받은 것만 쓴다.
func (s *subscription) waitInitial(timeout time.Duration) []string {
	select {
	case <-s.ready:
	case <-time.After(timeout):
		s.log.Warn("initial targets timed out, proceeding with defaults")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	enabled := make([]string, 0, len(s.targets))
	for target, ok := range s.targets {
		if ok {
			enabled = append(enabled, target)
		}
	}
	if len(enabled) == 0 {
		enabled = []string{"aws/ec2"}
	}
	return enabled
}

func (s *subscription) markReady() {
	s.once.Do(func() { close(s.ready) })
}

func report(
	ctx context.Context,
	c collectorv1.CollectorServiceClient,
	agentID string,
	targets []string,
	batchCount, perBatch int,
) (*collectorv1.ReportSummary, error) {
	stream, err := c.Report(ctx)
	if err != nil {
		return nil, err
	}

	for i := 0; i < batchCount; i++ {
		target := targets[i%len(targets)]
		batch := &collectorv1.MetricBatch{
			AgentId:     agentID,
			Target:      target,
			CollectedAt: timestamppb.Now(),
			Metrics:     generateMetrics(target, perBatch),
		}
		if err := stream.Send(batch); err != nil {
			return nil, fmt.Errorf("send batch %d: %w", i, err)
		}
	}

	return stream.CloseAndRecv()
}

func generateMetrics(target string, n int) []*collectorv1.Metric {
	metrics := make([]*collectorv1.Metric, 0, n)
	for i := 0; i < n; i++ {
		metrics = append(metrics, &collectorv1.Metric{
			Name:       "cpu_usage_percent",
			ResourceId: fmt.Sprintf("%s-%04d", target, i),
			Value:      rand.Float64() * 100,
			Labels: map[string]string{
				"region": "ap-northeast-2",
				"target": target,
			},
		})
	}
	return metrics
}
