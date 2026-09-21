package store

import "testing"

// 이 프로젝트에서 가장 중요한 로직이다.
//
// 쿠버네티스 워크로드 스펙에는 참조 대상이 "이름" 으로만 적혀 있다.
// 수집 시점에는 그 대상의 UID 를 알 수 없으므로, 전체 자원이 모인 뒤
// namespace + kind + name 역인덱스로 해석해야 한다.
func TestResolveRelations(t *testing.T) {
	s := New()
	s.UpsertResources([]*Resource{
		{UID: "cm-1", Kind: "ConfigMap", Name: "shop-config", Namespace: "shop"},
		{UID: "sec-1", Kind: "Secret", Name: "db-cred", Namespace: "shop"},
		{
			UID: "pod-1", Kind: "Pod", Name: "storefront-abc", Namespace: "shop",
			Relations: []Relation{
				{Type: "DEPENDENCY", TargetKind: "ConfigMap", TargetName: "shop-config", TargetNamespace: "shop"},
				{Type: "DEPENDENCY", TargetKind: "Secret", TargetName: "db-cred", TargetNamespace: "shop"},
				// 존재하지 않는 대상. 조용히 사라지지 않고 미해석으로 남아야 한다.
				{Type: "DEPENDENCY", TargetKind: "ConfigMap", TargetName: "ghost", TargetNamespace: "shop"},
			},
		},
	})

	resolved, unresolved := s.ResolveRelations()
	if resolved != 2 || unresolved != 1 {
		t.Fatalf("해석 결과 불일치: resolved=%d unresolved=%d, want 2/1", resolved, unresolved)
	}

	pod := findByUID(t, s, "pod-1")
	got := map[string]string{}
	for _, rel := range pod.Relations {
		got[rel.TargetName] = rel.TargetUID
	}
	if got["shop-config"] != "cm-1" {
		t.Errorf("ConfigMap 해석 실패: got %q", got["shop-config"])
	}
	if got["db-cred"] != "sec-1" {
		t.Errorf("Secret 해석 실패: got %q", got["db-cred"])
	}
	if got["ghost"] != "" {
		t.Errorf("없는 대상은 UID 가 비어야 함: got %q", got["ghost"])
	}
}

// 이름이 같아도 네임스페이스가 다르면 다른 자원이다.
// 이걸 구분하지 못하면 관계가 엉뚱한 곳으로 이어진다.
func TestResolveRelations_NamespaceIsolation(t *testing.T) {
	s := New()
	s.UpsertResources([]*Resource{
		{UID: "cm-shop", Kind: "ConfigMap", Name: "app-config", Namespace: "shop"},
		{UID: "cm-ops", Kind: "ConfigMap", Name: "app-config", Namespace: "ops"},
		{
			UID: "pod-ops", Kind: "Pod", Name: "worker", Namespace: "ops",
			Relations: []Relation{
				{Type: "DEPENDENCY", TargetKind: "ConfigMap", TargetName: "app-config", TargetNamespace: "ops"},
			},
		},
	})

	if resolved, _ := s.ResolveRelations(); resolved != 1 {
		t.Fatalf("resolved=%d, want 1", resolved)
	}
	pod := findByUID(t, s, "pod-ops")
	if uid := pod.Relations[0].TargetUID; uid != "cm-ops" {
		t.Errorf("같은 이름의 다른 네임스페이스 자원과 이어짐: got %q want cm-ops", uid)
	}
}

// 두 번째 수집에서 관계가 사라졌다면 이전 상태가 남아 있으면 안 된다.
func TestUpsertResources_Replaces(t *testing.T) {
	s := New()
	s.UpsertResources([]*Resource{{
		UID: "pod-1", Kind: "Pod", Name: "app", Namespace: "shop",
		Relations: []Relation{{TargetKind: "ConfigMap", TargetName: "old"}},
	}})
	s.UpsertResources([]*Resource{{
		UID: "pod-1", Kind: "Pod", Name: "app", Namespace: "shop",
	}})

	pod := findByUID(t, s, "pod-1")
	if len(pod.Relations) != 0 {
		t.Errorf("이전 관계가 남아 있음: %v", pod.Relations)
	}
	if total, _ := s.RelationStat(); total != 0 {
		t.Errorf("관계 통계가 갱신되지 않음: %d", total)
	}
}

// 메트릭은 무한히 쌓이면 안 된다. 보존 한도를 넘으면 오래된 것부터 버린다.
func TestAppendMetrics_RespectsLimit(t *testing.T) {
	s := New()
	batch := make([]MetricPoint, maxMetricPoints+120)
	for i := range batch {
		batch[i] = MetricPoint{Name: "cpu", ResourceUID: "u", Value: float64(i)}
	}
	s.AppendMetrics(batch)

	if got := len(s.metrics); got != maxMetricPoints {
		t.Fatalf("보존 한도 초과: %d, want %d", got, maxMetricPoints)
	}
	// 가장 오래된 120개가 밀려났는지 확인한다.
	if s.metrics[0].Value != 120 {
		t.Errorf("오래된 것부터 버려야 함: 첫 값 %v", s.metrics[0].Value)
	}
}

// 로그는 최신이 먼저 나와야 한다. 장애를 볼 때 최근 것부터 보기 때문이다.
func TestLogs_NewestFirstAndFiltered(t *testing.T) {
	s := New()
	s.AppendLogs([]LogLine{
		{PodUID: "p1", Namespace: "shop", Level: "info", Message: "첫째"},
		{PodUID: "p1", Namespace: "shop", Level: "error", Message: "둘째"},
		{PodUID: "p2", Namespace: "ops", Level: "error", Message: "셋째"},
	})

	all := s.Logs("", "", 10)
	if len(all) != 3 || all[0].Message != "셋째" {
		t.Errorf("최신이 먼저 나와야 함: %+v", all)
	}

	errs := s.Logs("", "error", 10)
	if len(errs) != 2 {
		t.Errorf("레벨 필터 실패: %d건", len(errs))
	}

	shop := s.Logs("shop", "", 10)
	if len(shop) != 2 {
		t.Errorf("네임스페이스 필터 실패: %d건", len(shop))
	}
}

func findByUID(t *testing.T, s *Store, uid string) Resource {
	t.Helper()
	for _, r := range s.Resources("", "") {
		if r.UID == uid {
			return r
		}
	}
	t.Fatalf("자원을 찾지 못함: %s", uid)
	return Resource{}
}
