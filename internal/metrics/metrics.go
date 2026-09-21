// Package metrics 는 수집기 자신의 상태를 Prometheus 로 노출한다.
//
// 수집기는 무인으로 돈다. 대시보드는 사람이 볼 때만 열리므로,
// "지금 잘 돌고 있는가" 를 기계가 판단할 수 있어야 한다.
//
// 수집한 값(파드 CPU 등)이 아니라 수집기 자체의 지표를 낸다.
// 둘을 섞으면 "수집기가 죽은 것" 과 "수집 대상이 조용한 것" 을 구분할 수 없다.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// BatchesReceived 는 수신한 배치 수다. kind 로 메타·메트릭·로그를 나눈다.
	BatchesReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "collector_batches_received_total",
		Help: "수신한 배치 수",
	}, []string{"kind"})

	// ItemsReceived 는 배치 안의 항목 수다.
	// 배치 수만 보면 한 배치에 몇 건이 들어왔는지 알 수 없다.
	ItemsReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "collector_items_received_total",
		Help: "수신한 항목 수",
	}, []string{"kind"})

	// ItemsRejected 는 검증에 실패해 버린 항목 수다.
	//
	// 이 값이 0 이 아니면 에이전트와 서버의 기대가 어긋났다는 뜻이다.
	// 조용히 버려지면 알 수 없으므로 반드시 지표로 낸다.
	ItemsRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "collector_items_rejected_total",
		Help: "검증 실패로 버린 항목 수",
	}, []string{"kind", "reason"})

	// StreamDuration 은 스트림 하나를 처리하는 데 걸린 시간이다.
	StreamDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "collector_stream_duration_seconds",
		Help:    "스트림 처리 소요 시간",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	}, []string{"kind"})

	// ResourcesTracked 는 현재 보관 중인 자원 수다. 종류별로 나눈다.
	ResourcesTracked = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "collector_resources_tracked",
		Help: "현재 보관 중인 자원 수",
	}, []string{"kind"})

	// RelationsTotal / RelationsResolved 는 관계 해석 상태다.
	//
	// 두 값이 벌어지면 수집 대상에서 빠진 자원 종류가 있다는 신호다.
	// 실제로 ReplicaSet 을 빼먹었을 때 이 차이로 알아챘다.
	RelationsTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "collector_relations_total",
		Help: "자원 간 관계 총 개수",
	})

	RelationsResolved = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "collector_relations_resolved",
		Help: "이름에서 UID 로 해석에 성공한 관계 수",
	})

	// LogEntriesByLevel 은 수집한 로그의 수준별 개수다.
	LogEntriesByLevel = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "collector_log_entries_by_level",
		Help: "보관 중인 로그의 수준별 개수",
	}, []string{"level"})

	// AgentLastSeen 은 에이전트가 마지막으로 전송한 시각(유닉스 초)이다.
	//
	// 게이지로 두면 Prometheus 에서 `time() - collector_agent_last_seen_seconds`
	// 로 "몇 초째 소식이 없는가" 를 바로 알 수 있다.
	AgentLastSeen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "collector_agent_last_seen_seconds",
		Help: "에이전트가 마지막으로 전송한 시각 (유닉스 초)",
	}, []string{"agent_id", "kind"})
)
