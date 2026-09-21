// Package collector 는 client-go 로 쿠버네티스 자원을 수집한다.
//
// 세 가지를 모은다.
//   - 메타  : 자원 스펙과 참조 관계
//   - 메트릭: metrics-server 가 제공하는 CPU · 메모리 사용량
//   - 로그  : 파드 컨테이너의 최근 로그 줄
package collector

import (
	"context"
	"fmt"
	"strconv"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// 관계 유형. 소유(계층)와 참조(의존)를 구분한다.
const (
	RelParentChild = "PARENT_CHILD"
	RelDependency  = "DEPENDENCY"
)

// CollectMeta 는 클러스터의 자원 메타데이터를 모은다.
//
// 수집 순서는 의미가 있다. 워크로드가 참조하는 ConfigMap · Secret ·
// ServiceAccount 를 먼저 모아야, 나중에 이름 → UID 해석이 가능하다.
func CollectMeta(ctx context.Context, cs kubernetes.Interface) ([]*collectorv1.ResourceMeta, error) {
	var out []*collectorv1.ResourceMeta

	nsList, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	for i := range nsList.Items {
		ns := &nsList.Items[i]
		out = append(out, &collectorv1.ResourceMeta{
			Uid:       string(ns.UID),
			Kind:      "Namespace",
			Name:      ns.Name,
			Labels:    ns.Labels,
			Status:    string(ns.Status.Phase),
			CreatedAt: timestamppb.New(ns.CreationTimestamp.Time),
		})
	}

	// 수집 대상 네임스페이스만 돈다. 쿠버네티스 시스템 네임스페이스까지
	// 넣으면 대시보드가 우리 워크로드를 못 보여준다.
	targets := targetNamespaces(nsList.Items)

	for _, ns := range targets {
		cms, err := cs.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list configmaps in %s: %w", ns, err)
		}
		for i := range cms.Items {
			c := &cms.Items[i]
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(c.UID),
				Kind:      "ConfigMap",
				Name:      c.Name,
				Namespace: c.Namespace,
				Labels:    c.Labels,
				Status:    "Active",
				CreatedAt: timestamppb.New(c.CreationTimestamp.Time),
				Attributes: map[string]string{
					"keys": strconv.Itoa(len(c.Data)),
				},
				Relations: []*collectorv1.Relation{
					nsParent(c.Namespace),
				},
			})
		}

		// Secret 은 값을 수집하지 않는다. 메타데이터만 본다.
		secs, err := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list secrets in %s: %w", ns, err)
		}
		for i := range secs.Items {
			s := &secs.Items[i]
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(s.UID),
				Kind:      "Secret",
				Name:      s.Name,
				Namespace: s.Namespace,
				Labels:    s.Labels,
				Status:    "Active",
				CreatedAt: timestamppb.New(s.CreationTimestamp.Time),
				Attributes: map[string]string{
					"type": string(s.Type),
					"keys": strconv.Itoa(len(s.Data)),
				},
				Relations: []*collectorv1.Relation{
					nsParent(s.Namespace),
				},
			})
		}

		sas, err := cs.CoreV1().ServiceAccounts(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list serviceaccounts in %s: %w", ns, err)
		}
		for i := range sas.Items {
			sa := &sas.Items[i]
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(sa.UID),
				Kind:      "ServiceAccount",
				Name:      sa.Name,
				Namespace: sa.Namespace,
				Labels:    sa.Labels,
				Status:    "Active",
				CreatedAt: timestamppb.New(sa.CreationTimestamp.Time),
				Relations: []*collectorv1.Relation{
					nsParent(sa.Namespace),
				},
			})
		}

		deps, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
		}
		for i := range deps.Items {
			d := &deps.Items[i]
			rels := []*collectorv1.Relation{nsParent(d.Namespace)}
			rels = append(rels, podSpecRefs(d.Namespace, &d.Spec.Template.Spec)...)

			status := "Progressing"
			if d.Status.ReadyReplicas == *d.Spec.Replicas {
				status = "Available"
			}
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(d.UID),
				Kind:      "Deployment",
				Name:      d.Name,
				Namespace: d.Namespace,
				Labels:    d.Labels,
				Status:    status,
				CreatedAt: timestamppb.New(d.CreationTimestamp.Time),
				Attributes: map[string]string{
					"replicas": strconv.Itoa(int(*d.Spec.Replicas)),
					"ready":    strconv.Itoa(int(d.Status.ReadyReplicas)),
					"image":    firstImage(&d.Spec.Template.Spec),
				},
				Relations: rels,
			})
		}

		// DaemonSet · StatefulSet 도 파드를 소유한다.
		//
		// Deployment 와 달리 중간에 ReplicaSet 이 없어 파드가 이들을 바로
		// 가리킨다. 빼면 그 파드의 부모가 해석되지 않는다.
		// 실제로 이 수집기의 node-agent 를 DaemonSet 으로 올렸을 때
		// collector_relations_total 과 resolved 가 108/107 로 벌어져 드러났다.
		dss, err := cs.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list daemonsets in %s: %w", ns, err)
		}
		for i := range dss.Items {
			ds := &dss.Items[i]
			rels := []*collectorv1.Relation{nsParent(ds.Namespace)}
			rels = append(rels, podSpecRefs(ds.Namespace, &ds.Spec.Template.Spec)...)
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(ds.UID),
				Kind:      "DaemonSet",
				Name:      ds.Name,
				Namespace: ds.Namespace,
				Labels:    ds.Labels,
				Status:    replicaStatus(ds.Status.NumberReady, ds.Status.DesiredNumberScheduled),
				CreatedAt: timestamppb.New(ds.CreationTimestamp.Time),
				Attributes: map[string]string{
					// DaemonSet 은 replicas 가 없다. 노드 수가 곧 기대치다.
					"desired": strconv.Itoa(int(ds.Status.DesiredNumberScheduled)),
					"ready":   strconv.Itoa(int(ds.Status.NumberReady)),
					"image":   firstImage(&ds.Spec.Template.Spec),
				},
				Relations: rels,
			})
		}

		sts, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list statefulsets in %s: %w", ns, err)
		}
		for i := range sts.Items {
			st := &sts.Items[i]
			rels := []*collectorv1.Relation{nsParent(st.Namespace)}
			rels = append(rels, podSpecRefs(st.Namespace, &st.Spec.Template.Spec)...)
			desired := int32(0)
			if st.Spec.Replicas != nil {
				desired = *st.Spec.Replicas
			}
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(st.UID),
				Kind:      "StatefulSet",
				Name:      st.Name,
				Namespace: st.Namespace,
				Labels:    st.Labels,
				Status:    replicaStatus(st.Status.ReadyReplicas, desired),
				CreatedAt: timestamppb.New(st.CreationTimestamp.Time),
				Attributes: map[string]string{
					"replicas": strconv.Itoa(int(desired)),
					"ready":    strconv.Itoa(int(st.Status.ReadyReplicas)),
					"image":    firstImage(&st.Spec.Template.Spec),
				},
				Relations: rels,
			})
		}

		// ReplicaSet · Job 을 수집해야 소유 체인이 끊기지 않는다.
		// 파드의 ownerReferences 는 Deployment 가 아니라 ReplicaSet 을 가리키고,
		// CronJob 이 만든 파드는 Job 을 가리킨다. 중간 단계를 빼면
		// Deployment -> Pod 를 이을 수 없다.
		rss, err := cs.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list replicasets in %s: %w", ns, err)
		}
		for i := range rss.Items {
			rs := &rss.Items[i]
			rels := []*collectorv1.Relation{nsParent(rs.Namespace)}
			for _, o := range rs.OwnerReferences {
				rels = append(rels, &collectorv1.Relation{
					Type:            RelParentChild,
					TargetKind:      o.Kind,
					TargetName:      o.Name,
					TargetNamespace: rs.Namespace,
					TargetUid:       string(o.UID),
				})
			}
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(rs.UID),
				Kind:      "ReplicaSet",
				Name:      rs.Name,
				Namespace: rs.Namespace,
				Labels:    rs.Labels,
				Status:    replicaStatus(rs.Status.ReadyReplicas, rs.Status.Replicas),
				CreatedAt: timestamppb.New(rs.CreationTimestamp.Time),
				Attributes: map[string]string{
					"replicas": strconv.Itoa(int(rs.Status.Replicas)),
					"ready":    strconv.Itoa(int(rs.Status.ReadyReplicas)),
				},
				Relations: rels,
			})
		}

		cjs, err := cs.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list cronjobs in %s: %w", ns, err)
		}
		for i := range cjs.Items {
			cj := &cjs.Items[i]
			rels := []*collectorv1.Relation{nsParent(cj.Namespace)}
			rels = append(rels, podSpecRefs(cj.Namespace, &cj.Spec.JobTemplate.Spec.Template.Spec)...)
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(cj.UID),
				Kind:      "CronJob",
				Name:      cj.Name,
				Namespace: cj.Namespace,
				Labels:    cj.Labels,
				Status:    "Scheduled",
				CreatedAt: timestamppb.New(cj.CreationTimestamp.Time),
				Attributes: map[string]string{
					"schedule": cj.Spec.Schedule,
					"active":   strconv.Itoa(len(cj.Status.Active)),
				},
				Relations: rels,
			})
		}

		jobs, err := cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list jobs in %s: %w", ns, err)
		}
		for i := range jobs.Items {
			j := &jobs.Items[i]
			rels := []*collectorv1.Relation{nsParent(j.Namespace)}
			for _, o := range j.OwnerReferences {
				rels = append(rels, &collectorv1.Relation{
					Type:            RelParentChild,
					TargetKind:      o.Kind,
					TargetName:      o.Name,
					TargetNamespace: j.Namespace,
					TargetUid:       string(o.UID),
				})
			}
			rels = append(rels, podSpecRefs(j.Namespace, &j.Spec.Template.Spec)...)
			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(j.UID),
				Kind:      "Job",
				Name:      j.Name,
				Namespace: j.Namespace,
				Labels:    j.Labels,
				Status:    jobStatus(j.Status.Succeeded, j.Status.Failed, j.Status.Active),
				CreatedAt: timestamppb.New(j.CreationTimestamp.Time),
				Attributes: map[string]string{
					"succeeded": strconv.Itoa(int(j.Status.Succeeded)),
					"failed":    strconv.Itoa(int(j.Status.Failed)),
				},
				Relations: rels,
			})
		}

		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			rels := []*collectorv1.Relation{nsParent(p.Namespace)}

			// 소유자(ReplicaSet · Job 등)는 ownerReferences 에 UID 가 이미 들어 있다.
			// 이름 해석이 필요 없는 유일한 경우다.
			for _, o := range p.OwnerReferences {
				rels = append(rels, &collectorv1.Relation{
					Type:            RelParentChild,
					TargetKind:      o.Kind,
					TargetName:      o.Name,
					TargetNamespace: p.Namespace,
					TargetUid:       string(o.UID),
				})
			}
			rels = append(rels, podSpecRefs(p.Namespace, &p.Spec)...)

			out = append(out, &collectorv1.ResourceMeta{
				Uid:       string(p.UID),
				Kind:      "Pod",
				Name:      p.Name,
				Namespace: p.Namespace,
				Labels:    p.Labels,
				Status:    string(p.Status.Phase),
				CreatedAt: timestamppb.New(p.CreationTimestamp.Time),
				Attributes: map[string]string{
					"node":       p.Spec.NodeName,
					"containers": strconv.Itoa(len(p.Spec.Containers)),
					"image":      firstImage(&p.Spec),
				},
				Relations: rels,
			})
		}
	}

	return out, nil
}

// podSpecRefs 는 파드 스펙이 참조하는 자원을 관계로 뽑는다.
//
// 여기가 이 수집기의 핵심이다. 쿠버네티스에서 자원 간 참조는 UID 가 아니라
// "이름" 으로 적힌다. envFrom.configMapRef.name 에는 이름만 있고,
// 그 ConfigMap 의 UID 는 스펙 어디에도 없다.
// 그래서 namespace + kind + name 을 관계에 함께 담아 보내고,
// 서버가 전체 자원을 받은 뒤 역인덱스로 UID 를 해석한다.
func podSpecRefs(ns string, spec *corev1.PodSpec) []*collectorv1.Relation {
	var rels []*collectorv1.Relation
	seen := make(map[string]bool)

	add := func(kind, name string) {
		if name == "" {
			return
		}
		key := kind + "|" + name
		if seen[key] {
			return
		}
		seen[key] = true
		rels = append(rels, &collectorv1.Relation{
			Type:            RelDependency,
			TargetKind:      kind,
			TargetName:      name,
			TargetNamespace: ns,
			// TargetUid 는 비워 둔다. 서버가 해석한다.
		})
	}

	if spec.ServiceAccountName != "" && spec.ServiceAccountName != "default" {
		add("ServiceAccount", spec.ServiceAccountName)
	}

	containers := append([]corev1.Container{}, spec.InitContainers...)
	containers = append(containers, spec.Containers...)

	for _, c := range containers {
		for _, ef := range c.EnvFrom {
			if ef.ConfigMapRef != nil {
				add("ConfigMap", ef.ConfigMapRef.Name)
			}
			if ef.SecretRef != nil {
				add("Secret", ef.SecretRef.Name)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
				add("ConfigMap", r.Name)
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil {
				add("Secret", r.Name)
			}
		}
	}

	for _, v := range spec.Volumes {
		if v.ConfigMap != nil {
			add("ConfigMap", v.ConfigMap.Name)
		}
		if v.Secret != nil {
			add("Secret", v.Secret.SecretName)
		}
	}

	return rels
}

func nsParent(ns string) *collectorv1.Relation {
	return &collectorv1.Relation{
		Type:       RelParentChild,
		TargetKind: "Namespace",
		TargetName: ns,
		// Namespace 는 클러스터 스코프라 TargetNamespace 가 비어 있다.
	}
}

func replicaStatus(ready, total int32) string {
	if total > 0 && ready == total {
		return "Available"
	}
	return "Progressing"
}

func jobStatus(succeeded, failed, active int32) string {
	switch {
	case failed > 0:
		return "Failed"
	case active > 0:
		return "Active"
	case succeeded > 0:
		return "Complete"
	}
	return "Pending"
}

func firstImage(spec *corev1.PodSpec) string {
	if len(spec.Containers) == 0 {
		return ""
	}
	return spec.Containers[0].Image
}

// targetNamespaces 는 시스템 네임스페이스를 제외한 목록을 준다.
func targetNamespaces(items []corev1.Namespace) []string {
	skip := map[string]bool{
		"kube-system":        true,
		"kube-public":        true,
		"kube-node-lease":    true,
		"local-path-storage": true,
	}
	out := make([]string, 0, len(items))
	for i := range items {
		if !skip[items[i].Name] {
			out = append(out, items[i].Name)
		}
	}
	return out
}
