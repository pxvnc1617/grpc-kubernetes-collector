// Package api 는 수집 결과를 조회하는 HTTP/JSON 엔드포인트다.
//
// 수집 자체는 gRPC 로 받고, 조회는 HTTP 로 낸다.
// 전송과 조회는 요구가 다르기 때문이다.
//   - 전송: 대량 · 단방향 · 스트리밍 → gRPC
//   - 조회: 소량 · 요청응답 · 브라우저에서 직접 호출 → HTTP/JSON
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/store"
)

type API struct {
	store *store.Store
}

func New(st *store.Store) *API { return &API{store: st} }

func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/summary", a.summary)
	mux.HandleFunc("GET /api/resources", a.resources)
	mux.HandleFunc("GET /api/metrics", a.metrics)
	mux.HandleFunc("GET /api/metrics/series", a.metricSeries)
	mux.HandleFunc("GET /api/logs", a.logs)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	// Prometheus 스크레이프 경로. /api 아래가 아니라 관례대로 /metrics 에 둔다.
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

// ── 요약 ─────────────────────────────────────────────────────

type summaryResponse struct {
	Stats     store.Stats    `json:"stats"`
	ByKind    map[string]int `json:"byKind"`
	ByNs      map[string]int `json:"byNamespace"`
	ByLevel   map[string]int `json:"byLevel"`
	Relations relationStat   `json:"relations"`
}

type relationStat struct {
	Total    int `json:"total"`
	Resolved int `json:"resolved"`
}

func (a *API) summary(w http.ResponseWriter, _ *http.Request) {
	total, resolved := a.store.RelationStat()
	writeJSON(w, summaryResponse{
		Stats:     a.store.Stats(),
		ByKind:    a.store.KindCount(),
		ByNs:      a.store.NamespaceCount(),
		ByLevel:   a.store.LevelCount(),
		Relations: relationStat{Total: total, Resolved: resolved},
	})
}

// ── 자원 ─────────────────────────────────────────────────────

func (a *API) resources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, a.store.Resources(q.Get("namespace"), q.Get("kind")))
}

// ── 메트릭 ───────────────────────────────────────────────────

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "cpu_usage_millicores"
	}
	writeJSON(w, a.store.LatestMetrics(name))
}

func (a *API) metricSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uid := q.Get("uid")
	if uid == "" {
		http.Error(w, `{"error":"uid is required"}`, http.StatusBadRequest)
		return
	}
	name := q.Get("name")
	if name == "" {
		name = "cpu_usage_millicores"
	}
	writeJSON(w, a.store.MetricSeries(uid, name))
}

// ── 로그 ─────────────────────────────────────────────────────

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	writeJSON(w, a.store.Logs(q.Get("namespace"), q.Get("level"), limit))
}

// ── 공통 ─────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 개발 중 Vite 개발 서버(5173)에서 직접 호출할 수 있게 허용한다.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
	}
}
