package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// bufconn 으로 실제 서버를 메모리 위에 띄운다.
// 포트를 잡지 않으므로 CI 에서 충돌이나 타이밍 문제가 없고,
// 목(mock)이 아니라 진짜 gRPC 스택을 통과한다.
func newTestClient(t *testing.T) (collectorv1.CollectorServiceClient, *store.Store) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	st := store.New()
	srv := grpc.NewServer()
	collectorv1.RegisterCollectorServiceServer(srv, New(slog.New(slog.NewTextHandler(io.Discard, nil)), st))

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

	return collectorv1.NewCollectorServiceClient(conn), st
}

func TestHealth_RequiresAgentID(t *testing.T) {
	c, _ := newTestClient(t)

	if _, err := c.Health(context.Background(), &collectorv1.HealthRequest{}); err == nil {
		t.Error("agent_id 가 없으면 거절해야 함")
	}
	res, err := c.Health(context.Background(), &collectorv1.HealthRequest{AgentId: "a1"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !res.GetOk() || res.GetServerVersion() == "" {
		t.Errorf("응답이 비었음: %+v", res)
	}
}

// 이 프로젝트에서 가장 중요한 성질이다.
//
// 수집기는 무인으로 돌기 때문에 대상 하나가 잘못됐다고 전체 전송이
// 멈추면 안 된다. 잘못된 배치는 거절 카운트만 올리고 스트림은 살린다.
func TestReportMeta_RejectsInvalidButKeepsStream(t *testing.T) {
	c, st := newTestClient(t)

	stream, err := c.ReportMeta(context.Background())
	if err != nil {
		t.Fatalf("ReportMeta: %v", err)
	}

	good := func(uid, name string) *collectorv1.MetaBatch {
		return &collectorv1.MetaBatch{
			AgentId: "a1", Cluster: "test",
			Resources: []*collectorv1.ResourceMeta{
				{Uid: uid, Kind: "ConfigMap", Name: name, Namespace: "shop"},
			},
			CollectedAt: timestamppb.Now(),
		}
	}

	send := []*collectorv1.MetaBatch{
		good("cm-1", "first"),
		// cluster 누락 → 거절
		{AgentId: "a1", Resources: []*collectorv1.ResourceMeta{{Uid: "x", Kind: "Pod", Name: "x"}}, CollectedAt: timestamppb.Now()},
		// uid 누락 → 거절
		{AgentId: "a1", Cluster: "test", Resources: []*collectorv1.ResourceMeta{{Kind: "Pod", Name: "y"}}, CollectedAt: timestamppb.Now()},
		// 미래 시각 → 거절. 시계가 어긋난 노드의 데이터를 그대로 믿지 않는다.
		{AgentId: "a1", Cluster: "test",
			Resources:   []*collectorv1.ResourceMeta{{Uid: "z", Kind: "Pod", Name: "z"}},
			CollectedAt: timestamppb.New(time.Now().Add(2 * time.Hour))},
		good("cm-2", "second"), // 거절 뒤에도 계속 흘러야 한다
	}
	for i, b := range send {
		if err := stream.Send(b); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}

	sum, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	if sum.GetBatchCount() != 2 {
		t.Errorf("정상 배치 수: got %d want 2", sum.GetBatchCount())
	}
	if sum.GetRejected() != 3 {
		t.Errorf("거절 수: got %d want 3", sum.GetRejected())
	}
	// 거절된 것은 저장되지 않아야 한다.
	if got := len(st.Resources("", "")); got != 2 {
		t.Errorf("저장된 자원 수: got %d want 2", got)
	}
}

// 관계 해석은 스트림이 닫힌 뒤에 일어나야 한다.
// 참조 대상이 같은 배치에 없을 수 있기 때문이다.
func TestReportMeta_ResolvesRelationsAfterStreamCloses(t *testing.T) {
	c, st := newTestClient(t)

	stream, err := c.ReportMeta(context.Background())
	if err != nil {
		t.Fatalf("ReportMeta: %v", err)
	}

	// 파드를 먼저 보낸다. 이 시점에 ConfigMap 은 아직 서버에 없다.
	if err := stream.Send(&collectorv1.MetaBatch{
		AgentId: "a1", Cluster: "test", CollectedAt: timestamppb.Now(),
		Resources: []*collectorv1.ResourceMeta{{
			Uid: "pod-1", Kind: "Pod", Name: "app", Namespace: "shop",
			Relations: []*collectorv1.Relation{{
				Type: "DEPENDENCY", TargetKind: "ConfigMap",
				TargetName: "late-config", TargetNamespace: "shop",
			}},
		}},
	}); err != nil {
		t.Fatalf("Send pod: %v", err)
	}

	// 참조 대상은 나중 배치에 온다.
	if err := stream.Send(&collectorv1.MetaBatch{
		AgentId: "a1", Cluster: "test", CollectedAt: timestamppb.Now(),
		Resources: []*collectorv1.ResourceMeta{{
			Uid: "cm-late", Kind: "ConfigMap", Name: "late-config", Namespace: "shop",
		}},
	}); err != nil {
		t.Fatalf("Send configmap: %v", err)
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	total, resolved := st.RelationStat()
	if total != 1 || resolved != 1 {
		t.Fatalf("뒤늦게 온 참조가 해석되지 않음: total=%d resolved=%d", total, resolved)
	}
}

func TestReportLog_CountsAndStores(t *testing.T) {
	c, st := newTestClient(t)

	stream, err := c.ReportLog(context.Background())
	if err != nil {
		t.Fatalf("ReportLog: %v", err)
	}
	if err := stream.Send(&collectorv1.LogBatch{
		AgentId: "a1", Cluster: "test", CollectedAt: timestamppb.Now(),
		Entries: []*collectorv1.LogEntry{
			{PodUid: "p1", PodName: "app", Namespace: "shop", Level: "error", Message: "boom", Timestamp: timestamppb.Now()},
			{PodUid: "p1", PodName: "app", Namespace: "shop", Level: "info", Message: "ok", Timestamp: timestamppb.Now()},
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// message 누락 → 배치 전체 거절
	if err := stream.Send(&collectorv1.LogBatch{
		AgentId: "a1", Cluster: "test", CollectedAt: timestamppb.Now(),
		Entries: []*collectorv1.LogEntry{{PodUid: "p2", PodName: "x"}},
	}); err != nil {
		t.Fatalf("Send invalid: %v", err)
	}

	sum, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if sum.GetItemCount() != 2 || sum.GetRejected() != 1 {
		t.Errorf("집계 불일치: items=%d rejected=%d", sum.GetItemCount(), sum.GetRejected())
	}
	if got := st.LevelCount()["error"]; got != 1 {
		t.Errorf("레벨 집계 실패: error=%d", got)
	}
}

// 서버가 수집 주기를 쥐고 있어야 에이전트를 건드리지 않고 빈도를 바꿀 수 있다.
func TestSubscribe_PushesTargetsWithServerSideInterval(t *testing.T) {
	c, _ := newTestClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := c.Subscribe(ctx, &collectorv1.SubscribeRequest{
		AgentId:      "a1",
		Capabilities: []string{"meta/all", "metric/pod", "log/pod"},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	got := map[string]int32{}
	for range 3 {
		t0, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got[t0.GetTarget()] = t0.GetIntervalSeconds()
	}

	want := map[string]int32{"meta/all": 30, "metric/pod": 15, "log/pod": 20}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s 주기: got %d want %d", k, got[k], v)
		}
	}
}

func TestValidateMeta(t *testing.T) {
	now := timestamppb.Now()
	ok := []*collectorv1.ResourceMeta{{Uid: "u", Kind: "Pod", Name: "n"}}

	cases := []struct {
		name  string
		batch *collectorv1.MetaBatch
		want  string
	}{
		{"정상", &collectorv1.MetaBatch{AgentId: "a", Cluster: "c", Resources: ok, CollectedAt: now}, ""},
		{"agent_id 누락", &collectorv1.MetaBatch{Cluster: "c", Resources: ok, CollectedAt: now}, "missing agent_id"},
		{"cluster 누락", &collectorv1.MetaBatch{AgentId: "a", Resources: ok, CollectedAt: now}, "missing cluster"},
		{"빈 자원", &collectorv1.MetaBatch{AgentId: "a", Cluster: "c", CollectedAt: now}, "empty resources"},
		{"시각 누락", &collectorv1.MetaBatch{AgentId: "a", Cluster: "c", Resources: ok}, "missing collected_at"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateMeta(c.batch); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}
