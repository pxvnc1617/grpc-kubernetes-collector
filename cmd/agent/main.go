package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/collector"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

func main() {
	addr := flag.String("addr", "localhost:50051", "collector server address")
	agentID := flag.String("agent-id", "agent-local", "agent identifier")
	cluster := flag.String("cluster", "kind-grpc-k8s-collector", "cluster identifier")
	kubeconfig := flag.String("kubeconfig", defaultKubeconfig(), "kubeconfig path (empty = in-cluster)")
	batchSize := flag.Int("batch", 50, "items per batch")
	once := flag.Bool("once", false, "collect once and exit")
	connectTimeout := flag.Duration("connect-timeout", 2*time.Minute, "how long to wait for the server on startup")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cs, mc, err := buildClients(*kubeconfig)
	if err != nil {
		log.Error("kubernetes client init failed", "err", err)
		os.Exit(1)
	}

	conn, err := grpc.NewClient(*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		log.Error("dial failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := collectorv1.NewCollectorServiceClient(conn)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 서버가 아직 안 떴을 수 있으므로 물러나며 재시도한다.
	//
	// 처음에는 실패 즉시 종료했는데, 쿠버네티스에 올리자 에이전트가 서버보다
	// 먼저 떠서 매번 한 번씩 죽고 재시작됐다. 재시작으로 넘어가긴 하지만
	// 서버가 잠깐 재기동될 때마다 CrashLoopBackOff 로 빠져 복구가 늦어진다.
	// 기다리는 쪽이 맞다.
	if err := waitForServer(ctx, log, client, *agentID, *connectTimeout); err != nil {
		log.Error("server unreachable", "err", err, "waited", connectTimeout.String())
		os.Exit(1)
	}

	// 구독은 별도 고루틴으로 돌린다.
	//
	// 서버는 Subscribe 스트림을 푸시용으로 계속 열어 두므로 io.EOF 가 오지 않는다.
	// 이걸 동기로 읽으면 영원히 블록되고, 전송과 컨텍스트를 공유하면
	// 전송까지 같은 데드라인에 걸린다. 그래서 분리한다.
	sub := newSubscription(log)
	go sub.run(ctx, client, *agentID)
	targets := sub.waitInitial(2 * time.Second)
	log.Info("collect targets", "targets", targets)

	run := func() {
		collectOnce(ctx, log, client, cs, mc, *agentID, *cluster, *batchSize)
	}

	run()
	if *once {
		return
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("agent stopped")
			return
		case <-ticker.C:
			run()
		}
	}
}

// collectOnce 는 메타 · 메트릭 · 로그를 한 차례 수집해 전송한다.
//
// 세 가지를 병렬로 돌리지 않는다. 같은 API 서버를 동시에 때리면
// 수집기가 클러스터에 부담을 주는 쪽이 되기 때문이다.
func collectOnce(
	ctx context.Context,
	log *slog.Logger,
	client collectorv1.CollectorServiceClient,
	cs kubernetes.Interface,
	mc metricsv.Interface,
	agentID, cluster string,
	batchSize int,
) {
	// ── 메타 ──────────────────────────────────────────────────
	metaCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	resources, err := collector.CollectMeta(metaCtx, cs)
	cancel()
	if err != nil {
		log.Error("meta collect failed", "err", err)
	} else if sum, err := sendMeta(ctx, client, agentID, cluster, resources, batchSize); err != nil {
		log.Error("meta send failed", "err", err)
	} else {
		log.Info("meta sent",
			"resources", len(resources),
			"batches", sum.GetBatchCount(), "rejected", sum.GetRejected(),
			"elapsed_ms", sum.GetElapsedMs())
	}

	// ── 메트릭 ────────────────────────────────────────────────
	metricCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	metrics, err := collector.CollectMetrics(metricCtx, mc, cs)
	cancel()
	if err != nil {
		// metrics-server 가 아직 준비 중일 수 있다. 치명적으로 보지 않는다.
		log.Warn("metric collect skipped", "err", err)
	} else if len(metrics) > 0 {
		if sum, err := sendMetric(ctx, client, agentID, cluster, metrics, batchSize); err != nil {
			log.Error("metric send failed", "err", err)
		} else {
			log.Info("metric sent",
				"metrics", len(metrics),
				"batches", sum.GetBatchCount(), "rejected", sum.GetRejected(),
				"elapsed_ms", sum.GetElapsedMs())
		}
	}

	// ── 로그 ──────────────────────────────────────────────────
	logCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	entries, err := collector.CollectLogs(logCtx, cs, 20)
	cancel()
	if err != nil {
		log.Error("log collect failed", "err", err)
	} else if len(entries) > 0 {
		if sum, err := sendLog(ctx, client, agentID, cluster, entries, batchSize); err != nil {
			log.Error("log send failed", "err", err)
		} else {
			log.Info("log sent",
				"entries", len(entries),
				"batches", sum.GetBatchCount(), "rejected", sum.GetRejected(),
				"elapsed_ms", sum.GetElapsedMs())
		}
	}
}

// waitForServer 는 서버가 응답할 때까지 물러나며 재시도한다.
// 간격은 1초에서 시작해 두 배씩 늘리고 15초에서 멈춘다.
func waitForServer(
	ctx context.Context,
	log *slog.Logger,
	c collectorv1.CollectorServiceClient,
	agentID string,
	limit time.Duration,
) error {
	deadline := time.Now().Add(limit)
	backoff := time.Second
	var lastErr error

	for attempt := 1; ; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		hr, err := c.Health(callCtx, &collectorv1.HealthRequest{AgentId: agentID})
		cancel()

		if err == nil {
			log.Info("connected", "server_version", hr.GetServerVersion(), "attempts", attempt)
			return nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return lastErr
		}
		log.Warn("server not ready, retrying",
			"attempt", attempt, "retry_in", backoff.String(), "err", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// ══════════════ 전송 ════════════════════════════════════════

func sendMeta(ctx context.Context, c collectorv1.CollectorServiceClient, agentID, cluster string, items []*collectorv1.ResourceMeta, size int) (*collectorv1.ReportSummary, error) {
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stream, err := c.ReportMeta(sendCtx)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		if err := stream.Send(&collectorv1.MetaBatch{
			AgentId:     agentID,
			Cluster:     cluster,
			Resources:   items[start:end],
			CollectedAt: timestamppb.Now(),
		}); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

func sendMetric(ctx context.Context, c collectorv1.CollectorServiceClient, agentID, cluster string, items []*collectorv1.Metric, size int) (*collectorv1.ReportSummary, error) {
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stream, err := c.ReportMetric(sendCtx)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		if err := stream.Send(&collectorv1.MetricBatch{
			AgentId:     agentID,
			Cluster:     cluster,
			Metrics:     items[start:end],
			CollectedAt: timestamppb.Now(),
		}); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

func sendLog(ctx context.Context, c collectorv1.CollectorServiceClient, agentID, cluster string, items []*collectorv1.LogEntry, size int) (*collectorv1.ReportSummary, error) {
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stream, err := c.ReportLog(sendCtx)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		if err := stream.Send(&collectorv1.LogBatch{
			AgentId:     agentID,
			Cluster:     cluster,
			Entries:     items[start:end],
			CollectedAt: timestamppb.Now(),
		}); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

// ══════════════ 구독 ════════════════════════════════════════

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

func (s *subscription) run(ctx context.Context, c collectorv1.CollectorServiceClient, agentID string) {
	stream, err := c.Subscribe(ctx, &collectorv1.SubscribeRequest{
		AgentId:      agentID,
		Capabilities: []string{"meta/all", "metric/pod", "log/pod"},
	})
	if err != nil {
		s.log.Error("subscribe failed", "err", err)
		s.markReady()
		return
	}

	// 첫 배치를 받으면 waitInitial 을 풀어준다.
	// 이후에도 스트림은 유지되어 서버의 대상 변경을 계속 받는다.
	for {
		t, err := stream.Recv()
		if err != nil {
			s.markReady()
			return
		}
		s.mu.Lock()
		s.targets[t.GetTarget()] = t.GetEnabled()
		s.mu.Unlock()
		s.markReady()
	}
}

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
		enabled = []string{"meta/all", "metric/pod", "log/pod"}
	}
	return enabled
}

func (s *subscription) markReady() {
	s.once.Do(func() { close(s.ready) })
}

// ══════════════ 클라이언트 ══════════════════════════════════

func buildClients(kubeconfig string) (*kubernetes.Clientset, *metricsv.Clientset, error) {
	var cfg *rest.Config
	var err error

	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, nil, err
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	mc, err := metricsv.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cs, mc, nil
}

func defaultKubeconfig() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}
