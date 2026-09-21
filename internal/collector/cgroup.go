package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
)

// cgroup 수집은 metrics-server 경로와 성격이 다르다.
//
//	metrics-server  API 서버 경유 · 클러스터 전체 · 15초 집계값 · Deployment 1개
//	cgroup          노드 로컬 · 그 노드의 파드만 · 커널이 세는 원본 · DaemonSet
//
// metrics-server 는 kubelet 이 이미 가공한 값을 준다. 집계 주기가 있어
// 짧게 튀는 사용량은 뭉개진다. cgroup 은 커널 카운터를 직접 읽으므로
// 원본에 가깝고, metrics-server 가 없는 클러스터에서도 동작한다.
//
// 대신 노드에 직접 접근해야 하므로 DaemonSet 으로 노드마다 떠야 한다.

// CgroupRoot 는 노드의 cgroup 파일시스템 경로다.
// DaemonSet 에서는 hostPath 로 마운트한 경로를 넣는다.
const defaultCgroupRoot = "/host/sys/fs/cgroup"

// ContainerUsage 는 컨테이너 하나의 cgroup 사용량이다.
type ContainerUsage struct {
	PodUID      string // 경로에서 복원한 파드 UID
	ContainerID string // cri-containerd-<id> 의 <id>
	CPUNanos    uint64 // cpuacct.usage — 부팅 이후 누적 CPU 시간(ns)
	MemoryBytes uint64 // memory.usage_in_bytes — 현재 사용량
	MemoryLimit uint64 // memory.limit_in_bytes — 제한이 없으면 매우 큰 값
}

// CollectCgroup 은 노드의 cgroup 계층을 훑어 컨테이너별 사용량을 모은다.
//
// 지원 범위는 cgroup v1 + systemd 드라이버다. 경로가 이렇게 생겼다.
//
//	<root>/memory/kubelet.slice/kubelet-kubepods.slice/
//	  kubelet-kubepods-besteffort.slice/
//	    kubelet-kubepods-besteffort-pod<UID with _>.slice/
//	      cri-containerd-<containerID>.scope/memory.usage_in_bytes
//
// 파드 UID 는 경로에서 하이픈이 밑줄로 바뀐 채 들어 있다. 되돌려야
// 쿠버네티스가 아는 UID 와 이어진다.
func CollectCgroup(root string) ([]ContainerUsage, error) {
	if root == "" {
		root = defaultCgroupRoot
	}

	memRoot := filepath.Join(root, "memory")
	if _, err := os.Stat(memRoot); err != nil {
		return nil, fmt.Errorf("cgroup v1 memory controller not found at %s: %w", memRoot, err)
	}

	var out []ContainerUsage

	// 파드 슬라이스는 QoS 계층(besteffort/burstable) 아래 또는 바로 아래에 있다.
	// 깊이를 고정하지 않고 "pod<UID>.slice" 디렉터리를 찾아 내려간다.
	err := filepath.WalkDir(memRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// 권한이 없거나 방금 사라진 cgroup 은 건너뛴다.
			// 하나 때문에 전체 수집을 멈추지 않는다.
			return nil //nolint:nilerr
		}
		if !d.IsDir() {
			return nil
		}

		podUID := podUIDFromSlice(d.Name())
		if podUID == "" {
			return nil
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			containerID := containerIDFromScope(e.Name())
			if containerID == "" {
				continue
			}

			memDir := filepath.Join(path, e.Name())
			// cpuacct 는 별도 컨트롤러라 경로의 /memory/ 만 바꿔 준다.
			cpuDir := strings.Replace(memDir, string(os.PathSeparator)+"memory"+string(os.PathSeparator),
				string(os.PathSeparator)+"cpuacct"+string(os.PathSeparator), 1)

			u := ContainerUsage{PodUID: podUID, ContainerID: containerID}
			u.MemoryBytes = readUint(filepath.Join(memDir, "memory.usage_in_bytes"))
			u.MemoryLimit = readUint(filepath.Join(memDir, "memory.limit_in_bytes"))
			u.CPUNanos = readUint(filepath.Join(cpuDir, "cpuacct.usage"))

			// 셋 다 0 이면 방금 만들어졌거나 읽지 못한 것이다. 담지 않는다.
			if u.MemoryBytes == 0 && u.CPUNanos == 0 {
				continue
			}
			out = append(out, u)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// podUIDFromSlice 는 슬라이스 디렉터리 이름에서 파드 UID 를 되살린다.
//
//	kubelet-kubepods-besteffort-pod6d93af47_0ee3_4b96_bb9a_8952ed07be29.slice
//	                            └─────────────── 이 부분 ───────────────┘
//	→ 6d93af47-0ee3-4b96-bb9a-8952ed07be29
//
// systemd 는 슬라이스 이름에 하이픈을 못 쓰므로 밑줄로 바꿔 넣는다.
// 되돌리지 않으면 쿠버네티스가 아는 UID 와 이어지지 않는다.
func podUIDFromSlice(name string) string {
	if !strings.HasSuffix(name, ".slice") {
		return ""
	}
	// "pod" 만 찾으면 "kubelet-kubepods.slice" 의 kube*pod*s 에도 걸린다.
	// 파드 슬라이스는 반드시 "-pod" 로 구분되므로 그 경계를 쓴다.
	idx := strings.LastIndex(name, "-pod")
	if idx < 0 {
		return ""
	}
	uid := strings.TrimSuffix(name[idx+len("-pod"):], ".slice")

	// UID 형식이 아니면 파드 슬라이스가 아니다.
	// QoS 계층("-besteffort")처럼 생긴 것을 걸러낸다.
	if !looksLikeUID(uid) {
		return ""
	}
	return strings.ReplaceAll(uid, "_", "-")
}

// looksLikeUID 는 쿠버네티스 UID(8-4-4-4-12 의 16진수) 형태인지 본다.
// 경로에서는 하이픈이 밑줄로 바뀌어 있으므로 둘 다 허용한다.
func looksLikeUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '_' && r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// containerIDFromScope 는 스코프 이름에서 컨테이너 ID 를 뽑는다.
//
//	cri-containerd-91f8a99d1752....scope → 91f8a99d1752...
//
// 런타임마다 접두사가 다르다(containerd, crio, docker). 알려진 것을 벗겨낸다.
func containerIDFromScope(name string) string {
	if !strings.HasSuffix(name, ".scope") {
		return ""
	}
	id := strings.TrimSuffix(name, ".scope")
	for _, prefix := range []string{"cri-containerd-", "crio-", "docker-"} {
		if strings.HasPrefix(id, prefix) {
			return strings.TrimPrefix(id, prefix)
		}
	}
	return ""
}

func readUint(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ToMetrics 는 cgroup 사용량을 전송용 메트릭으로 바꾼다.
//
// metrics-server 경로와 이름을 구분한다. 같은 이름을 쓰면 두 값이 섞여
// 어느 경로로 잰 값인지 알 수 없게 된다.
func ToMetrics(usages []ContainerUsage, nodeName string, podName func(uid string) (name, namespace string)) []*collectorv1.Metric {
	out := make([]*collectorv1.Metric, 0, len(usages)*2)
	for _, u := range usages {
		name, ns := podName(u.PodUID)
		if name == "" {
			// 파드 목록에 없는 cgroup = 방금 사라진 컨테이너.
			continue
		}
		labels := map[string]string{
			"node":         nodeName,
			"pod":          name,
			"namespace":    ns,
			"container_id": shortID(u.ContainerID),
			"source":       "cgroup",
		}
		out = append(out,
			&collectorv1.Metric{
				Name:         "cgroup_cpu_usage_nanoseconds_total",
				ResourceUid:  u.PodUID,
				ResourceName: name,
				Namespace:    ns,
				Value:        float64(u.CPUNanos),
				Labels:       labels,
			},
			&collectorv1.Metric{
				Name:         "cgroup_memory_usage_bytes",
				ResourceUid:  u.PodUID,
				ResourceName: name,
				Namespace:    ns,
				Value:        float64(u.MemoryBytes),
				Labels:       labels,
			},
		)
	}
	return out
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
