package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-metric-collector/gen/collector/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// newTestClient 는 bufconn 으로 실제 네트워크 없이 gRPC 스택을 태운다.
func newTestClient(t *testing.T) (collectorv1.CollectorServiceClient, *Collector) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))

	srv := grpc.NewServer()
	collectorv1.RegisterCollectorServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return collectorv1.NewCollectorServiceClient(conn), svc
}

func batch(agentID, target string, n int) *collectorv1.MetricBatch {
	metrics := make([]*collectorv1.Metric, 0, n)
	for i := 0; i < n; i++ {
		metrics = append(metrics, &collectorv1.Metric{
			Name:       "cpu_usage_percent",
			ResourceId: "res-1",
			Value:      float64(i),
		})
	}
	return &collectorv1.MetricBatch{
		AgentId:     agentID,
		Target:      target,
		CollectedAt: timestamppb.Now(),
		Metrics:     metrics,
	}
}

func TestHealth_RequiresAgentID(t *testing.T) {
	client, _ := newTestClient(t)

	if _, err := client.Health(context.Background(), &collectorv1.HealthRequest{}); err == nil {
		t.Fatal("expected error when agent_id is empty")
	}

	res, err := client.Health(context.Background(), &collectorv1.HealthRequest{AgentId: "a1"})
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !res.GetOk() {
		t.Fatal("expected ok=true")
	}
}

func TestReport_CountsMetrics(t *testing.T) {
	client, _ := newTestClient(t)

	stream, err := client.Report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := stream.Send(batch("a1", "aws/ec2", 10)); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	summary, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, want := summary.GetBatchCount(), int64(5); got != want {
		t.Errorf("batch_count = %d, want %d", got, want)
	}
	if got, want := summary.GetMetricCount(), int64(50); got != want {
		t.Errorf("metric_count = %d, want %d", got, want)
	}
	if summary.GetRejected() != 0 {
		t.Errorf("rejected = %d, want 0", summary.GetRejected())
	}
}

// 잘못된 배치가 섞여도 스트림 전체가 죽지 않고, 나머지는 정상 집계되어야 한다.
// 수집기는 일부 대상이 실패해도 나머지가 계속 흘러야 하기 때문이다.
func TestReport_RejectsInvalidButKeepsStream(t *testing.T) {
	client, _ := newTestClient(t)

	stream, err := client.Report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	_ = stream.Send(batch("a1", "aws/ec2", 10)) // 정상
	bad := batch("a1", "", 3)                   // target 누락
	_ = stream.Send(bad)
	_ = stream.Send(batch("a1", "k8s/pod", 7)) // 정상

	summary, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, want := summary.GetBatchCount(), int64(2); got != want {
		t.Errorf("batch_count = %d, want %d", got, want)
	}
	if got, want := summary.GetMetricCount(), int64(17); got != want {
		t.Errorf("metric_count = %d, want %d", got, want)
	}
	if got, want := summary.GetRejected(), int64(3); got != want {
		t.Errorf("rejected = %d, want %d", got, want)
	}
}

func TestSubscribe_ReturnsCapabilitiesAsTargets(t *testing.T) {
	client, _ := newTestClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, &collectorv1.SubscribeRequest{
		AgentId:      "a1",
		Capabilities: []string{"aws/ec2", "k8s/pod"},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	var got []string
	for i := 0; i < 2; i++ {
		target, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		got = append(got, target.GetTarget())
	}

	if len(got) != 2 || got[0] != "aws/ec2" || got[1] != "k8s/pod" {
		t.Errorf("targets = %v, want [aws/ec2 k8s/pod]", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		batch *collectorv1.MetricBatch
		valid bool
	}{
		{"정상", batch("a1", "aws/ec2", 1), true},
		{"agent_id 누락", batch("", "aws/ec2", 1), false},
		{"target 누락", batch("a1", "", 1), false},
		{"메트릭 없음", batch("a1", "aws/ec2", 0), false},
		{"collected_at 누락", &collectorv1.MetricBatch{
			AgentId: "a1", Target: "aws/ec2",
			Metrics: []*collectorv1.Metric{{Name: "n", ResourceId: "r"}},
		}, false},
		{"미래 시각", &collectorv1.MetricBatch{
			AgentId: "a1", Target: "aws/ec2",
			CollectedAt: timestamppb.New(time.Now().Add(2 * time.Hour)),
			Metrics:     []*collectorv1.Metric{{Name: "n", ResourceId: "r"}},
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := validate(tt.batch)
			if got := reason == ""; got != tt.valid {
				t.Errorf("validate() = %q, valid = %v, want valid = %v", reason, got, tt.valid)
			}
		})
	}
}
