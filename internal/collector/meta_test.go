package collector

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// podSpecRefs 가 참조를 빠짐없이, 그리고 중복 없이 뽑는지 본다.
//
// 이 함수가 틀리면 관계가 조용히 누락된다. 코드는 정상 동작하고
// 대시보드에는 관계가 몇 개 적게 보일 뿐이라 알아채기 어렵다.
// 그래서 참조가 들어올 수 있는 네 경로를 모두 테스트에 넣는다.
func TestPodSpecRefs(t *testing.T) {
	spec := &corev1.PodSpec{
		ServiceAccountName: "runner",
		InitContainers: []corev1.Container{{
			Name: "init",
			EnvFrom: []corev1.EnvFromSource{{
				ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "init-config"},
				},
			}},
		}},
		Containers: []corev1.Container{{
			Name: "app",
			EnvFrom: []corev1.EnvFromSource{
				{ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"},
				}},
				{SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "db-cred"},
				}},
			},
			Env: []corev1.EnvVar{
				{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "db-cred"}, // 중복
						Key:                  "token",
					},
				}},
				{Name: "MODE", ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "mode-config"},
						Key:                  "mode",
					},
				}},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "cfg", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "vol-config"},
				},
			}},
			{Name: "sec", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "vol-secret"},
			}},
		},
	}

	got := make([]string, 0)
	for _, r := range podSpecRefs("shop", spec) {
		if r.GetType() != RelDependency {
			t.Errorf("참조 관계는 DEPENDENCY 여야 함: got %s", r.GetType())
		}
		if r.GetTargetNamespace() != "shop" {
			t.Errorf("네임스페이스가 붙어야 함: got %q", r.GetTargetNamespace())
		}
		if r.GetTargetUid() != "" {
			t.Error("수집 시점에는 UID 를 알 수 없으므로 비어 있어야 함")
		}
		got = append(got, r.GetTargetKind()+"/"+r.GetTargetName())
	}
	sort.Strings(got)

	want := []string{
		"ConfigMap/app-config",
		"ConfigMap/init-config",
		"ConfigMap/mode-config",
		"ConfigMap/vol-config",
		"Secret/db-cred", // env 와 envFrom 양쪽에 있지만 한 번만
		"Secret/vol-secret",
		"ServiceAccount/runner",
	}
	if len(got) != len(want) {
		t.Fatalf("참조 개수 불일치\n got=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got=%s want=%s", i, got[i], want[i])
		}
	}
}

// default 서비스어카운트는 모든 파드에 자동으로 붙으므로 관계로 세지 않는다.
// 세면 클러스터의 모든 파드가 같은 곳을 가리켜 관계도가 의미를 잃는다.
func TestPodSpecRefs_SkipsDefaultServiceAccount(t *testing.T) {
	for _, sa := range []string{"", "default"} {
		spec := &corev1.PodSpec{ServiceAccountName: sa}
		if got := podSpecRefs("shop", spec); len(got) != 0 {
			t.Errorf("serviceAccountName=%q 는 관계가 없어야 함: got %d", sa, len(got))
		}
	}
}

// CollectMeta 가 시스템 네임스페이스를 건너뛰고,
// 소유 관계(ownerReferences)는 UID 를 그대로 쓰는지 본다.
func TestCollectMeta(t *testing.T) {
	cs := fake.NewSimpleClientset(
		ns("shop"),
		ns("kube-system"), // 수집 대상에서 빠져야 한다
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-config", Namespace: "shop", UID: "cm-uid"},
			Data:       map[string]string{"A": "1", "B": "2"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "storefront-abc", Namespace: "shop", UID: "pod-uid",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "ReplicaSet", Name: "storefront-rs", UID: "rs-uid"},
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "app",
					Image: "busybox:1.36",
					EnvFrom: []corev1.EnvFromSource{{
						ConfigMapRef: &corev1.ConfigMapEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "shop-config"},
						},
					}},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system", UID: "sys-uid"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)

	got, err := CollectMeta(context.Background(), cs)
	if err != nil {
		t.Fatalf("CollectMeta: %v", err)
	}

	byUID := make(map[string]string) // uid -> kind
	for _, r := range got {
		byUID[r.GetUid()] = r.GetKind()
	}

	if _, ok := byUID["sys-uid"]; ok {
		t.Error("kube-system 의 파드는 수집되지 않아야 함")
	}
	if byUID["pod-uid"] != "Pod" || byUID["cm-uid"] != "ConfigMap" {
		t.Errorf("대상 자원이 빠짐: %v", byUID)
	}
	// Namespace 는 시스템 것도 목록에는 남는다. 계층의 뿌리이기 때문이다.
	if byUID[""] == "" && len(got) < 4 {
		t.Errorf("네임스페이스가 수집되지 않음: %d건", len(got))
	}

	var pod *struct {
		ownerUID string
		depName  string
	}
	for _, r := range got {
		if r.GetUid() != "pod-uid" {
			continue
		}
		pod = &struct {
			ownerUID string
			depName  string
		}{}
		for _, rel := range r.GetRelations() {
			switch rel.GetTargetKind() {
			case "ReplicaSet":
				pod.ownerUID = rel.GetTargetUid()
			case "ConfigMap":
				pod.depName = rel.GetTargetName()
			}
		}
	}
	if pod == nil {
		t.Fatal("파드를 찾지 못함")
	}
	// ownerReferences 에는 UID 가 이미 있으므로 해석이 필요 없다.
	if pod.ownerUID != "rs-uid" {
		t.Errorf("소유 관계 UID 가 그대로 쓰여야 함: got %q", pod.ownerUID)
	}
	// 참조는 이름만 있으므로 UID 는 서버가 채운다.
	if pod.depName != "shop-config" {
		t.Errorf("ConfigMap 참조가 빠짐: got %q", pod.depName)
	}
}

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: metav1UID(name)},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
}

func metav1UID(s string) types.UID { return types.UID("ns-" + s) }
