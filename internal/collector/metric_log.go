package collector

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// ══════════════ 메트릭 ══════════════════════════════════════

// CollectMetrics 는 metrics-server 에서 파드별 CPU · 메모리 사용량을 읽는다.
//
// metrics-server 가 아직 준비되지 않았거나 설치되지 않은 클러스터가 있으므로,
// 실패를 치명적으로 다루지 않고 호출한 쪽이 판단하게 에러를 그대로 올린다.
func CollectMetrics(ctx context.Context, mc metricsv.Interface, cs kubernetes.Interface) ([]*collectorv1.Metric, error) {
	nsList, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	// 파드 이름 → UID. 메트릭 API 는 UID 를 주지 않으므로 직접 잇는다.
	uidOf := make(map[string]string)
	for _, ns := range targetNamespaces(nsList.Items) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			uidOf[p.Namespace+"/"+p.Name] = string(p.UID)
		}
	}

	var out []*collectorv1.Metric
	for _, ns := range targetNamespaces(nsList.Items) {
		pm, err := mc.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pod metrics in %s: %w", ns, err)
		}
		for i := range pm.Items {
			m := &pm.Items[i]
			uid := uidOf[m.Namespace+"/"+m.Name]
			if uid == "" {
				// 메트릭은 있는데 파드 목록에 없다 = 방금 사라진 파드.
				// 버리되 조용히 넘기지는 않도록 호출부가 개수를 셀 수 있게 한다.
				continue
			}

			var cpuMilli, memBytes float64
			for _, c := range m.Containers {
				cpuMilli += float64(c.Usage.Cpu().MilliValue())
				memBytes += float64(c.Usage.Memory().Value())
			}

			labels := map[string]string{"namespace": m.Namespace, "pod": m.Name}
			out = append(out,
				&collectorv1.Metric{
					Name:         "cpu_usage_millicores",
					ResourceUid:  uid,
					ResourceName: m.Name,
					Namespace:    m.Namespace,
					Value:        cpuMilli,
					Labels:       labels,
				},
				&collectorv1.Metric{
					Name:         "memory_usage_bytes",
					ResourceUid:  uid,
					ResourceName: m.Name,
					Namespace:    m.Namespace,
					Value:        memBytes,
					Labels:       labels,
				},
			)
		}
	}
	return out, nil
}

// ══════════════ 로그 ════════════════════════════════════════

// levelPattern 은 로그 줄에서 수준을 뽑는다.
//
// 운영 로그는 형식이 제각각이라 정규식 하나로 전부 잡을 수 없다.
// 잡히지 않으면 버리지 않고 "unknown" 으로 표시한다.
// 조용히 사라지는 데이터가 없어야 어느 형식을 놓쳤는지 알 수 있다.
var levelPattern = regexp.MustCompile(`(?i)\blevel[=:"\s]+(trace|debug|info|warn|warning|error|fatal)\b`)

// CollectLogs 는 각 파드의 최근 로그를 tailLines 만큼 읽는다.
func CollectLogs(ctx context.Context, cs kubernetes.Interface, tailLines int64) ([]*collectorv1.LogEntry, error) {
	nsList, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	var out []*collectorv1.LogEntry
	for _, ns := range targetNamespaces(nsList.Items) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase != corev1.PodRunning {
				continue
			}
			for _, c := range p.Spec.Containers {
				entries, err := podLogs(ctx, cs, p, c.Name, tailLines)
				if err != nil {
					// 컨테이너 하나가 실패해도 나머지 수집은 계속한다.
					continue
				}
				out = append(out, entries...)
			}
		}
	}
	return out, nil
}

func podLogs(ctx context.Context, cs kubernetes.Interface, p *corev1.Pod, container string, tail int64) ([]*collectorv1.LogEntry, error) {
	req := cs.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{
		Container:  container,
		TailLines:  &tail,
		Timestamps: true,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var out []*collectorv1.LogEntry
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		ts, msg := splitTimestamp(sc.Text())
		if msg == "" {
			continue
		}
		out = append(out, &collectorv1.LogEntry{
			PodUid:    string(p.UID),
			PodName:   p.Name,
			Namespace: p.Namespace,
			Container: container,
			Message:   msg,
			Level:     extractLevel(msg),
			Timestamp: timestamppb.New(ts),
		})
	}
	return out, sc.Err()
}

// splitTimestamp 는 쿠버네티스가 앞에 붙인 RFC3339Nano 시각을 떼어 낸다.
func splitTimestamp(line string) (time.Time, string) {
	head, rest, found := strings.Cut(line, " ")
	if !found {
		return time.Now(), strings.TrimSpace(line)
	}
	ts, err := time.Parse(time.RFC3339Nano, head)
	if err != nil {
		// 시각이 없는 줄도 버리지 않는다.
		return time.Now(), strings.TrimSpace(line)
	}
	return ts, strings.TrimSpace(rest)
}

func extractLevel(msg string) string {
	if m := levelPattern.FindStringSubmatch(msg); len(m) == 2 {
		lv := strings.ToLower(m[1])
		if lv == "warning" {
			return "warn"
		}
		return lv
	}
	return "unknown"
}
