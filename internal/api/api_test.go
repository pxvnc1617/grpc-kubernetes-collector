package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/store"
)

func seeded() *store.Store {
	st := store.New()
	st.UpsertResources([]*store.Resource{
		{UID: "cm-1", Kind: "ConfigMap", Name: "shop-config", Namespace: "shop"},
		{UID: "pod-1", Kind: "Pod", Name: "storefront", Namespace: "shop", Status: "Running",
			Relations: []store.Relation{
				{Type: "DEPENDENCY", TargetKind: "ConfigMap", TargetName: "shop-config", TargetNamespace: "shop"},
				{Type: "DEPENDENCY", TargetKind: "Secret", TargetName: "ghost", TargetNamespace: "shop"},
			}},
		{UID: "pod-2", Kind: "Pod", Name: "worker", Namespace: "ops", Status: "Running"},
	})
	st.ResolveRelations()
	st.AppendMetrics([]store.MetricPoint{
		{Name: "cpu_usage_millicores", ResourceUID: "pod-1", ResourceName: "storefront", Namespace: "shop", Value: 120},
		{Name: "cpu_usage_millicores", ResourceUID: "pod-2", ResourceName: "worker", Namespace: "ops", Value: 30},
		{Name: "memory_usage_bytes", ResourceUID: "pod-1", ResourceName: "storefront", Namespace: "shop", Value: 8 << 20},
	})
	st.AppendLogs([]store.LogLine{
		{PodUID: "pod-1", PodName: "storefront", Namespace: "shop", Level: "error", Message: "boom"},
		{PodUID: "pod-2", PodName: "worker", Namespace: "ops", Level: "info", Message: "ok"},
	})
	st.RecordStream("meta", 1, 3, 0)
	return st
}

func do(t *testing.T, path string, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	New(seeded()).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s → %d", path, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type: %q", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("%s 응답 파싱 실패: %v", path, err)
	}
}

func TestSummary(t *testing.T) {
	var got summaryResponse
	do(t, "/api/summary", &got)

	if got.ByKind["Pod"] != 2 || got.ByKind["ConfigMap"] != 1 {
		t.Errorf("종류별 집계: %v", got.ByKind)
	}
	if got.ByNs["shop"] != 2 || got.ByNs["ops"] != 1 {
		t.Errorf("네임스페이스별 집계: %v", got.ByNs)
	}
	// 관계 2건 중 1건만 대상이 존재하므로 1건은 미해석으로 남아야 한다.
	if got.Relations.Total != 2 || got.Relations.Resolved != 1 {
		t.Errorf("관계 집계: %+v want total=2 resolved=1", got.Relations)
	}
	if got.ByLevel["error"] != 1 {
		t.Errorf("로그 레벨 집계: %v", got.ByLevel)
	}
	if got.Stats.Meta.Items != 3 {
		t.Errorf("스트림 통계: %+v", got.Stats.Meta)
	}
}

func TestResources_Filters(t *testing.T) {
	var all []store.Resource
	do(t, "/api/resources", &all)
	if len(all) != 3 {
		t.Fatalf("전체: %d건 want 3", len(all))
	}

	var shop []store.Resource
	do(t, "/api/resources?namespace=shop", &shop)
	if len(shop) != 2 {
		t.Errorf("네임스페이스 필터: %d건 want 2", len(shop))
	}

	var pods []store.Resource
	do(t, "/api/resources?kind=Pod", &pods)
	if len(pods) != 2 {
		t.Errorf("종류 필터: %d건 want 2", len(pods))
	}

	var both []store.Resource
	do(t, "/api/resources?namespace=ops&kind=Pod", &both)
	if len(both) != 1 || both[0].Name != "worker" {
		t.Errorf("복합 필터: %+v", both)
	}
}

// 메트릭은 기본값이 CPU 이고, 값이 큰 순으로 내려간다.
// 대시보드가 상위 사용량부터 보여주기 위해서다.
func TestMetrics_DefaultsToCPUAndSortsDesc(t *testing.T) {
	var got []store.MetricPoint
	do(t, "/api/metrics", &got)

	if len(got) != 2 {
		t.Fatalf("CPU 메트릭: %d건 want 2", len(got))
	}
	if got[0].Value < got[1].Value {
		t.Errorf("내림차순이 아님: %v %v", got[0].Value, got[1].Value)
	}
	for _, p := range got {
		if p.Name != "cpu_usage_millicores" {
			t.Errorf("기본값이 CPU 가 아님: %s", p.Name)
		}
	}

	var mem []store.MetricPoint
	do(t, "/api/metrics?name=memory_usage_bytes", &mem)
	if len(mem) != 1 {
		t.Errorf("메모리 메트릭: %d건 want 1", len(mem))
	}
}

func TestMetricSeries_RequiresUID(t *testing.T) {
	rec := httptest.NewRecorder()
	New(seeded()).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/metrics/series", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("uid 없으면 400 이어야 함: got %d", rec.Code)
	}

	var got []store.MetricPoint
	do(t, "/api/metrics/series?uid=pod-1", &got)
	if len(got) != 1 {
		t.Errorf("시계열: %d건 want 1", len(got))
	}
}

func TestLogs_Filters(t *testing.T) {
	var all []store.LogLine
	do(t, "/api/logs", &all)
	if len(all) != 2 {
		t.Fatalf("전체 로그: %d건", len(all))
	}

	var errs []store.LogLine
	do(t, "/api/logs?level=error", &errs)
	if len(errs) != 1 || errs[0].Message != "boom" {
		t.Errorf("레벨 필터: %+v", errs)
	}

	var limited []store.LogLine
	do(t, "/api/logs?limit=1", &limited)
	if len(limited) != 1 {
		t.Errorf("limit: %d건", len(limited))
	}
}

func TestHealthz(t *testing.T) {
	var got map[string]string
	do(t, "/api/healthz", &got)
	if got["status"] != "ok" {
		t.Errorf("healthz: %v", got)
	}
}
