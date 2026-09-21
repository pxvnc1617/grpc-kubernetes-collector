// Package store 는 수집된 결과를 메모리에 보관한다.
//
// 실제 제품이라면 적재기가 Kafka 를 소비해 MariaDB · OpenSearch · Prometheus 로
// 라우팅하지만, 이 프로젝트는 전송 계층과 수집 정확성을 보는 것이 목적이라
// 저장은 메모리로 한정했다. 대신 실제 저장소가 감당해야 할 성질
// — 최신 상태 유지, 보존 한도, 동시 접근 — 은 동일하게 다룬다.
package store

import (
	"sort"
	"sync"
	"time"
)

// ── 저장 모델 ────────────────────────────────────────────────

type Relation struct {
	Type            string `json:"type"`
	TargetKind      string `json:"targetKind"`
	TargetName      string `json:"targetName"`
	TargetNamespace string `json:"targetNamespace"`
	TargetUID       string `json:"targetUid"`
	Resolved        bool   `json:"resolved"` // 이름 → UID 해석 성공 여부
}

type Resource struct {
	UID        string            `json:"uid"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Status     string            `json:"status"`
	Labels     map[string]string `json:"labels,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Relations  []Relation        `json:"relations,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	SeenAt     time.Time         `json:"seenAt"` // 마지막 수집 시각
}

type MetricPoint struct {
	Name         string    `json:"name"`
	ResourceUID  string    `json:"resourceUid"`
	ResourceName string    `json:"resourceName"`
	Namespace    string    `json:"namespace"`
	Value        float64   `json:"value"`
	At           time.Time `json:"at"`
}

type LogLine struct {
	PodUID    string    `json:"podUid"`
	PodName   string    `json:"podName"`
	Namespace string    `json:"namespace"`
	Container string    `json:"container"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	At        time.Time `json:"at"`
}

// ── 수집 통계 ────────────────────────────────────────────────

type StreamStat struct {
	Batches  int64     `json:"batches"`
	Items    int64     `json:"items"`
	Rejected int64     `json:"rejected"`
	LastAt   time.Time `json:"lastAt"`
}

type Stats struct {
	Meta   StreamStat `json:"meta"`
	Metric StreamStat `json:"metric"`
	Log    StreamStat `json:"log"`
}

// ── 저장소 ───────────────────────────────────────────────────

const (
	maxMetricPoints = 2000 // 자원 하나가 아니라 전체 기준 보존 한도
	maxLogLines     = 1000
)

type Store struct {
	mu sync.RWMutex

	resources map[string]*Resource // uid -> 최신 상태
	metrics   []MetricPoint        // 오래된 것부터
	logs      []LogLine
	stats     Stats
}

func New() *Store {
	return &Store{resources: make(map[string]*Resource)}
}

// UpsertResources 는 수집된 자원을 최신 상태로 덮어쓴다.
// 같은 UID 가 다시 오면 교체하되 SeenAt 을 갱신해, 어떤 자원이
// 최근 수집에서 빠졌는지 구분할 수 있게 한다.
func (s *Store) UpsertResources(list []*Resource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, r := range list {
		r.SeenAt = now
		s.resources[r.UID] = r
	}
}

// ResolveRelations 는 이름으로만 적힌 참조를 UID 로 잇는다.
//
// 쿠버네티스 워크로드 스펙에는 configMapRef / secretRef 가 "이름" 으로만
// 들어 있다. 수집 시점에는 대상의 UID 를 알 수 없으므로, 저장소에 모두
// 모인 뒤 namespace + kind + name 으로 역인덱스를 만들어 해석한다.
func (s *Store) ResolveRelations() (resolved, unresolved int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := make(map[string]string, len(s.resources)) // ns|kind|name -> uid
	for uid, r := range s.resources {
		index[r.Namespace+"|"+r.Kind+"|"+r.Name] = uid
	}

	for _, r := range s.resources {
		for i := range r.Relations {
			rel := &r.Relations[i]
			if uid, ok := index[rel.TargetNamespace+"|"+rel.TargetKind+"|"+rel.TargetName]; ok {
				rel.TargetUID = uid
				rel.Resolved = true
				resolved++
			} else {
				rel.Resolved = false
				unresolved++
			}
		}
	}
	return resolved, unresolved
}

func (s *Store) AppendMetrics(points []MetricPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = append(s.metrics, points...)
	if over := len(s.metrics) - maxMetricPoints; over > 0 {
		s.metrics = append([]MetricPoint(nil), s.metrics[over:]...)
	}
}

func (s *Store) AppendLogs(lines []LogLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, lines...)
	if over := len(s.logs) - maxLogLines; over > 0 {
		s.logs = append([]LogLine(nil), s.logs[over:]...)
	}
}

func (s *Store) RecordStream(kind string, batches, items, rejected int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t *StreamStat
	switch kind {
	case "meta":
		t = &s.stats.Meta
	case "metric":
		t = &s.stats.Metric
	case "log":
		t = &s.stats.Log
	default:
		return
	}
	t.Batches += batches
	t.Items += items
	t.Rejected += rejected
	t.LastAt = time.Now()
}

// ── 조회 ─────────────────────────────────────────────────────

func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// Resources 는 네임스페이스·종류로 걸러 정렬된 목록을 준다.
// 빈 문자열은 필터를 적용하지 않는다는 뜻이다.
func (s *Store) Resources(namespace, kind string) []Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Resource, 0, len(s.resources))
	for _, r := range s.resources {
		if namespace != "" && r.Namespace != namespace {
			continue
		}
		if kind != "" && r.Kind != kind {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// KindCount 는 자원 종류별 개수를 준다. 대시보드 요약용.
func (s *Store) KindCount() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := make(map[string]int)
	for _, r := range s.resources {
		m[r.Kind]++
	}
	return m
}

// NamespaceCount 는 네임스페이스별 자원 개수를 준다.
func (s *Store) NamespaceCount() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := make(map[string]int)
	for _, r := range s.resources {
		ns := r.Namespace
		if ns == "" {
			ns = "(cluster)"
		}
		m[ns]++
	}
	return m
}

// RelationStat 은 관계 해석 성공·실패 수를 준다.
// 이름으로 적힌 참조가 실제 자원과 이어졌는지 보는 지표다.
func (s *Store) RelationStat() (total, resolved int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.resources {
		for _, rel := range r.Relations {
			total++
			if rel.Resolved {
				resolved++
			}
		}
	}
	return total, resolved
}

// LatestMetrics 는 자원별 최신 값만 남겨 준다.
func (s *Store) LatestMetrics(name string) []MetricPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	latest := make(map[string]MetricPoint)
	for _, p := range s.metrics {
		if name != "" && p.Name != name {
			continue
		}
		key := p.ResourceUID + "|" + p.Name
		if cur, ok := latest[key]; !ok || p.At.After(cur.At) {
			latest[key] = p
		}
	}
	out := make([]MetricPoint, 0, len(latest))
	for _, p := range latest {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out
}

// MetricSeries 는 특정 자원의 시계열을 오래된 순으로 준다.
func (s *Store) MetricSeries(uid, name string) []MetricPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]MetricPoint, 0, 64)
	for _, p := range s.metrics {
		if p.ResourceUID == uid && p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// Logs 는 최신 로그를 limit 개 준다. level 로 거를 수 있다.
func (s *Store) Logs(namespace, level string, limit int) []LogLine {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > maxLogLines {
		limit = 200
	}
	out := make([]LogLine, 0, limit)
	for i := len(s.logs) - 1; i >= 0 && len(out) < limit; i-- {
		l := s.logs[i]
		if namespace != "" && l.Namespace != namespace {
			continue
		}
		if level != "" && l.Level != level {
			continue
		}
		out = append(out, l)
	}
	return out
}

// LevelCount 는 로그 레벨별 개수를 준다.
func (s *Store) LevelCount() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := make(map[string]int)
	for _, l := range s.logs {
		m[l.Level]++
	}
	return m
}
