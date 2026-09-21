// node-agent 는 노드 하나의 cgroup 을 읽어 컨테이너 사용량을 수집한다.
//
// 클러스터 전체를 읽는 agent 와 달리 이쪽은 DaemonSet 으로 노드마다 뜬다.
// cgroup 파일시스템은 API 서버로 못 읽고 노드에 직접 접근해야 하기 때문이다.
//
//	agent       Deployment 1개   API 서버 경유 · 메타 · 로그 · metrics-server
//	node-agent  DaemonSet  N개   노드 로컬 · cgroup 원본 카운터
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/collector"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	addr := flag.String("addr", "localhost:50051", "collector server address")
	agentID := flag.String("agent-id", "node-agent", "agent identifier")
	cluster := flag.String("cluster", "kind-grpc-k8s-collector", "cluster identifier")
	nodeName := flag.String("node", os.Getenv("NODE_NAME"), "node this agent runs on")
	cgroupRoot := flag.String("cgroup-root", "/host/sys/fs/cgroup", "cgroup filesystem root")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path (empty = in-cluster)")
	interval := flag.Duration("interval", 15*time.Second, "collection interval")
	batchSize := flag.Int("batch", 50, "items per batch")
	once := flag.Bool("once", false, "collect once and exit")
	connectTimeout := flag.Duration("connect-timeout", 2*time.Minute, "how long to wait for the server on startup")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("node", *nodeName)

	cs, err := buildClient(*kubeconfig)
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

	if err := waitForServer(ctx, log, client, *agentID, *connectTimeout); err != nil {
		log.Error("server unreachable", "err", err)
		os.Exit(1)
	}

	run := func() { collectOnce(ctx, log, client, cs, *agentID, *cluster, *nodeName, *cgroupRoot, *batchSize) }

	run()
	if *once {
		return
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("node agent stopped")
			return
		case <-ticker.C:
			run()
		}
	}
}

func collectOnce(
	ctx context.Context,
	log *slog.Logger,
	client collectorv1.CollectorServiceClient,
	cs kubernetes.Interface,
	agentID, cluster, nodeName, cgroupRoot string,
	batchSize int,
) {
	usages, err := collector.CollectCgroup(cgroupRoot)
	if err != nil {
		log.Error("cgroup collect failed", "err", err, "root", cgroupRoot)
		return
	}

	// cgroup 은 파드 UID 만 준다. 이름과 네임스페이스는 API 서버에서 받아 잇는다.
	// 이 노드의 파드만 조회하면 되므로 fieldSelector 로 좁힌다.
	// 전체를 받아 걸러내면 노드 수만큼 API 서버 부하가 늘어난다.
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	pods, err := cs.CoreV1().Pods("").List(listCtx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	cancel()
	if err != nil {
		log.Error("list pods failed", "err", err)
		return
	}

	index := make(map[string][2]string, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		index[string(p.UID)] = [2]string{p.Name, p.Namespace}
	}
	lookup := func(uid string) (string, string) {
		v, ok := index[uid]
		if !ok {
			return "", ""
		}
		return v[0], v[1]
	}

	metrics := collector.ToMetrics(usages, nodeName, lookup)
	if len(metrics) == 0 {
		log.Warn("no cgroup metrics matched running pods", "cgroups", len(usages))
		return
	}

	sum, err := send(ctx, client, agentID, cluster, metrics, batchSize)
	if err != nil {
		log.Error("send failed", "err", err)
		return
	}
	log.Info("cgroup metrics sent",
		"containers", len(usages), "metrics", len(metrics),
		"batches", sum.GetBatchCount(), "rejected", sum.GetRejected(),
		"elapsed_ms", sum.GetElapsedMs())
}

func send(ctx context.Context, c collectorv1.CollectorServiceClient, agentID, cluster string, items []*collectorv1.Metric, size int) (*collectorv1.ReportSummary, error) {
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

// waitForServer 는 서버가 응답할 때까지 물러나며 재시도한다.
// agent 와 같은 이유다 — 기동 순서로 죽으면 CrashLoopBackOff 로 빠진다.
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
		log.Warn("server not ready, retrying", "attempt", attempt, "retry_in", backoff.String())

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

func buildClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
