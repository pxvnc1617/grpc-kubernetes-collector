package collector

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// 파드 UID 복원이 이 수집기의 핵심이다.
//
// systemd 는 슬라이스 이름에 하이픈을 못 쓰므로 밑줄로 바꿔 넣는다.
// 되돌리지 않으면 쿠버네티스가 아는 UID 와 이어지지 않고,
// 그러면 메트릭은 수집되는데 어느 파드 것인지 알 수 없게 된다.
//
// 아래 문자열은 실제 kind 노드에서 확인한 형식이다.
func TestPodUIDFromSlice(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"besteffort QoS",
			"kubelet-kubepods-besteffort-pod6d93af47_0ee3_4b96_bb9a_8952ed07be29.slice",
			"6d93af47-0ee3-4b96-bb9a-8952ed07be29",
		},
		{
			"burstable QoS",
			"kubelet-kubepods-burstable-pod8f93cd2e_06a2_413b_80bd_48da5f9c3ebb.slice",
			"8f93cd2e-06a2-413b-80bd-48da5f9c3ebb",
		},
		{
			"guaranteed — QoS 계층 없이 바로 붙는다",
			"kubelet-kubepods-pod394c3b24_5b20_424e_98ed_e12d623f2a87.slice",
			"394c3b24-5b20-424e-98ed-e12d623f2a87",
		},
		{"QoS 슬라이스 자체", "kubelet-kubepods-besteffort.slice", ""},
		{"kubepods 루트", "kubelet-kubepods.slice", ""},
		{"슬라이스가 아님", "cri-containerd-abc.scope", ""},
		{"빈 문자열", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := podUIDFromSlice(c.in); got != c.want {
				t.Errorf("podUIDFromSlice(%q)\n got  %q\n want %q", c.in, got, c.want)
			}
		})
	}
}

// 런타임마다 스코프 접두사가 다르다. 모르는 접두사는 담지 않는다.
// 잘못 벗겨내면 엉뚱한 문자열이 컨테이너 ID 로 들어간다.
func TestContainerIDFromScope(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cri-containerd-91f8a99d1752e9148b9015d73fe8720436f2e0a3bca29847daf122bf5b0decd9.scope",
			"91f8a99d1752e9148b9015d73fe8720436f2e0a3bca29847daf122bf5b0decd9"},
		{"crio-abc123.scope", "abc123"},
		{"docker-def456.scope", "def456"},
		{"unknown-runtime-xyz.scope", ""}, // 모르는 접두사는 건너뛴다
		{"kubelet-kubepods-besteffort.slice", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := containerIDFromScope(c.in); got != c.want {
			t.Errorf("containerIDFromScope(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

// 실제 cgroup v1 계층을 파일로 흉내 내어 전체 경로를 훑는지 본다.
// QoS 계층이 있는 것과 없는 것을 섞어 둔다.
func TestCollectCgroup(t *testing.T) {
	root := t.TempDir()

	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// besteffort 아래 컨테이너 둘
	podA := filepath.Join(root, "memory", "kubelet.slice", "kubelet-kubepods.slice",
		"kubelet-kubepods-besteffort.slice",
		"kubelet-kubepods-besteffort-pod6d93af47_0ee3_4b96_bb9a_8952ed07be29.slice")
	write(filepath.Join(podA, "cri-containerd-aaa111.scope", "memory.usage_in_bytes"), "200704")
	write(filepath.Join(podA, "cri-containerd-aaa111.scope", "memory.limit_in_bytes"), "67108864")
	write(filepath.Join(podA, "cri-containerd-bbb222.scope", "memory.usage_in_bytes"), "409600")

	// QoS 계층 없이 바로 붙은 파드
	podB := filepath.Join(root, "memory", "kubelet.slice", "kubelet-kubepods.slice",
		"kubelet-kubepods-pod394c3b24_5b20_424e_98ed_e12d623f2a87.slice")
	write(filepath.Join(podB, "cri-containerd-ccc333.scope", "memory.usage_in_bytes"), "1048576")

	// cpuacct 는 별도 컨트롤러 트리다
	cpuA := filepath.Join(root, "cpuacct", "kubelet.slice", "kubelet-kubepods.slice",
		"kubelet-kubepods-besteffort.slice",
		"kubelet-kubepods-besteffort-pod6d93af47_0ee3_4b96_bb9a_8952ed07be29.slice")
	write(filepath.Join(cpuA, "cri-containerd-aaa111.scope", "cpuacct.usage"), "8808500")

	// 값이 없는 컨테이너 — 방금 만들어진 것. 담지 않아야 한다.
	write(filepath.Join(podA, "cri-containerd-empty.scope", "memory.usage_in_bytes"), "0")

	got, err := CollectCgroup(root)
	if err != nil {
		t.Fatalf("CollectCgroup: %v", err)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].ContainerID < got[j].ContainerID })

	if len(got) != 3 {
		t.Fatalf("컨테이너 수: %d건 want 3\n%+v", len(got), got)
	}

	want := []ContainerUsage{
		{PodUID: "6d93af47-0ee3-4b96-bb9a-8952ed07be29", ContainerID: "aaa111",
			CPUNanos: 8808500, MemoryBytes: 200704, MemoryLimit: 67108864},
		{PodUID: "6d93af47-0ee3-4b96-bb9a-8952ed07be29", ContainerID: "bbb222",
			MemoryBytes: 409600},
		{PodUID: "394c3b24-5b20-424e-98ed-e12d623f2a87", ContainerID: "ccc333",
			MemoryBytes: 1048576},
	}
	for i, w := range want {
		g := got[i]
		if g.PodUID != w.PodUID || g.ContainerID != w.ContainerID ||
			g.CPUNanos != w.CPUNanos || g.MemoryBytes != w.MemoryBytes {
			t.Errorf("[%d]\n got  %+v\n want %+v", i, g, w)
		}
	}
}

// cgroup v1 이 없는 환경에서는 에러를 돌려주고, 호출한 쪽이 판단하게 한다.
// 조용히 빈 결과를 주면 "수집 대상이 없는 것" 과 구분되지 않는다.
func TestCollectCgroup_MissingController(t *testing.T) {
	if _, err := CollectCgroup(t.TempDir()); err == nil {
		t.Error("memory 컨트롤러가 없으면 에러여야 함")
	}
}

// 파드 목록에 없는 cgroup 은 방금 사라진 컨테이너다. 메트릭으로 내보내지 않는다.
func TestToMetrics_SkipsUnknownPods(t *testing.T) {
	usages := []ContainerUsage{
		{PodUID: "known", ContainerID: "c1", CPUNanos: 100, MemoryBytes: 200},
		{PodUID: "vanished", ContainerID: "c2", CPUNanos: 300, MemoryBytes: 400},
	}
	lookup := func(uid string) (string, string) {
		if uid == "known" {
			return "storefront", "shop"
		}
		return "", ""
	}

	got := ToMetrics(usages, "node-1", lookup)
	if len(got) != 2 { // 알려진 파드 하나 × CPU·메모리
		t.Fatalf("메트릭 수: %d want 2", len(got))
	}
	for _, m := range got {
		if m.GetResourceName() != "storefront" || m.GetNamespace() != "shop" {
			t.Errorf("파드 정보가 안 붙음: %+v", m)
		}
		// metrics-server 경로와 이름이 겹치면 안 된다.
		if n := m.GetName(); n != "cgroup_cpu_usage_nanoseconds_total" && n != "cgroup_memory_usage_bytes" {
			t.Errorf("예상치 못한 메트릭 이름: %s", n)
		}
		if m.GetLabels()["source"] != "cgroup" {
			t.Errorf("source 라벨이 없음: %v", m.GetLabels())
		}
	}
}
