# k8s-collector

[![CI](https://github.com/pxvnc1617/grpc-kubernetes-collector/actions/workflows/ci.yml/badge.svg)](https://github.com/pxvnc1617/grpc-kubernetes-collector/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![gRPC](https://img.shields.io/badge/gRPC-streaming-244c5a?logo=grpc&logoColor=white)](https://grpc.io)
[![Vue](https://img.shields.io/badge/Vue-3-42b883?logo=vuedotjs&logoColor=white)](https://vuejs.org)

쿠버네티스 자원의 **메타데이터 · 메트릭데이터 · 로그데이터**를 수집해 gRPC 스트리밍으로
서버에 전송하고, 수집 현황을 대시보드로 보여준다.

```
kind 클러스터                수집 에이전트                 수집 서버              대시보드
──────────────              ──────────────               ──────────           ──────────
Deployment                   client-go
ReplicaSet    ──────────▶    ├ 메타    ──┐
Pod                          ├ 메트릭  ──┤ gRPC          검증 ─ 집계
ConfigMap                    └ 로그    ──┘ client         이름→UID 해석  ──▶  Vue 3 SPA
Secret                                     streaming      메모리 보관        HTTP/JSON
CronJob · Job                                  :50051                          :8080
```

**실무에서는 수집 결과를 Kafka 로 발행했다.** 메시지 브로커를 거치지 않고 에이전트와
서버가 직접 통신한다면 전송 계층을 어떻게 설계해야 하는지 확인해 보려고 만들었다.

---

## 바로 띄워 보기

```bash
make cluster      # kind 클러스터 + metrics-server + 샘플 워크로드
make run-server   # gRPC :50051, 대시보드 http://localhost:8080
make run-agent    # 20초 주기 수집 (다른 터미널)
```

`make run-server` 하나로 수집 서버와 대시보드가 같이 뜬다. 별도 웹 서버가 필요 없다.

---

## 무엇을 수집하는가

| 종류 | 대상 | 방법 |
|---|---|---|
| **메타데이터** | Namespace · Deployment · ReplicaSet · Pod · CronJob · Job · ConfigMap · Secret · ServiceAccount | `client-go` |
| **메트릭데이터** | 파드별 CPU(millicores) · 메모리(bytes) | `metrics-server` |
| **로그데이터** | 파드 컨테이너 최근 로그 + 수준 추출 | `GetLogs` |

Secret 은 **값을 수집하지 않는다.** 이름 · 타입 · 키 개수만 본다.

---

## 설계 의도

### 이름으로 적힌 참조를 UID 로 잇는다

이 프로젝트에서 가장 공들인 부분이다.

쿠버네티스에서 자원 간 참조는 **UID 가 아니라 "이름" 으로 적힌다.**
워크로드 스펙의 `envFrom.configMapRef.name` 에는 이름만 있고, 그 ConfigMap 의
UID 는 스펙 어디에도 없다. 수집 시점에는 대상의 UID 를 알 수 없다.

```go
// 수집 단계 — 이름만 담아 보낸다
&collectorv1.Relation{
    Type:            RelDependency,
    TargetKind:      "ConfigMap",
    TargetName:      "shop-config",
    TargetNamespace: "shop",
    // TargetUid 는 비워 둔다
}
```

그래서 **서버가 전체 자원을 받은 뒤** `namespace|kind|name` 역인덱스를 만들어 해석한다.
참조 대상이 같은 배치에 없을 수 있으므로, 해석은 **스트림이 닫힌 다음**에 일어난다.

참조가 들어올 수 있는 경로는 네 군데다. 하나라도 빠지면 관계가 조용히 누락된다.

- `envFrom.configMapRef` / `envFrom.secretRef`
- `env[].valueFrom.configMapKeyRef` / `secretKeyRef`
- `volumes[].configMap` / `volumes[].secret`
- `spec.serviceAccountName`

### 소유 관계와 참조 관계를 구분한다

| | 관계 | UID |
|---|---|---|
| **소유** `PARENT_CHILD` | Namespace → Pod, Deployment → ReplicaSet → Pod | `ownerReferences` 에 이미 있음 |
| **참조** `DEPENDENCY` | Pod → ConfigMap · Secret · ServiceAccount | **이름만 있음 → 해석 필요** |

ReplicaSet 과 Job 을 수집하지 않으면 소유 체인이 끊긴다.
파드의 `ownerReferences` 는 Deployment 가 아니라 **ReplicaSet** 을 가리키기 때문이다.

> 실제로 처음엔 ReplicaSet · Job 을 빼먹어 **6건이 미해석**으로 남았다.
> 대시보드에 미해석 개수가 보였기 때문에 찾을 수 있었다.

### 메타 · 메트릭 · 로그를 하나의 RPC 로 뭉개지 않는다

성격이 다르다. 메타는 스펙과 관계, 메트릭은 수치, 로그는 텍스트다.
검증 규칙도 달라야 한다.

```protobuf
rpc ReportMeta  (stream MetaBatch)   returns (ReportSummary);
rpc ReportMetric(stream MetricBatch) returns (ReportSummary);
rpc ReportLog   (stream LogBatch)    returns (ReportSummary);

rpc Subscribe(SubscribeRequest) returns (stream CollectTarget);  // 대상 푸시
rpc Health   (HealthRequest)    returns (HealthResponse);        // 기동 확인
```

### 부분 실패가 스트림 전체를 끊지 않는다

수집기는 무인으로 돈다. 대상 하나가 잘못됐다고 전체 전송이 멈추면 안 된다.

```go
if reason := validateMeta(batch); reason != "" {
    rejected += int64(len(batch.GetResources()))
    c.log.Warn("meta batch rejected", "reason", reason, ...)
    continue        // ← return 이 아니다. 스트림은 유지한다
}
```

거절된 개수는 버려지지 않고 `ReportSummary.rejected` 에 담겨 에이전트로 돌아간다.
**조용히 사라지는 데이터가 없어야** 무엇을 놓쳤는지 알 수 있다.

같은 이유로 로그 수준을 못 뽑은 줄도 버리지 않고 `unknown` 으로 남긴다.
버리면 어떤 형식을 놓쳤는지 영영 모른다.

### 수집 주기는 서버가 쥔다

에이전트를 재배포하지 않고 수집 빈도를 바꾸기 위해서다.
에이전트는 자신이 수집 **가능한** 대상을 알리고, 서버가 **주기와 활성 여부**를 내려준다.

구독 스트림은 서버가 푸시용으로 계속 열어 두므로 `io.EOF` 가 오지 않는다.
**전송과 컨텍스트를 공유하면 둘이 함께 데드라인에 걸린다.**
그래서 구독은 별도 고루틴으로 분리하고, 초기 목록만 `waitInitial(2s)` 로 기다린다.

---

## 검증

```bash
make test
```

| 패키지 | 커버리지 | 주요 테스트 |
|---|---|---|
| `internal/api` | 97.3% | 필터 · 정렬 · 집계 · 400 응답 |
| `internal/server` | 68.6% | **거절 후에도 스트림 생존**, 뒤늦게 온 참조 해석, 주기 푸시 |
| `internal/store` | 50.7% | 이름→UID 해석, **네임스페이스 격리**, 보존 한도 |
| `internal/collector` | 48.3% | **참조 4경로 추출 · 중복 제거**, 시스템 네임스페이스 제외, 로그 수준 |

- gRPC 는 **bufconn** 으로 메모리 위에 실제 서버를 띄워 테스트한다. 목이 아니라 진짜 스택을 통과한다.
- 쿠버네티스는 **fake clientset** 을 쓴다. 클러스터 없이 수집 로직을 검증한다.
- CI 는 거기서 멈추지 않고 **kind 클러스터를 실제로 띄워** 수집을 한 번 돌린다.
  자원 20건 이상, 거절 0건, **미해석 관계 0건**을 통과 조건으로 건다.

---

## 실행 결과

```
$ go run ./cmd/agent -once
{"msg":"connected","server_version":"v0.2.0"}
{"msg":"collect targets","targets":["meta/all","metric/pod","log/pod"]}
{"msg":"meta sent",  "resources":34,"batches":1,"rejected":0,"elapsed_ms":1}
{"msg":"metric sent","metrics":8,  "batches":1,"rejected":0,"elapsed_ms":0}
{"msg":"log sent",   "entries":80, "batches":2,"rejected":0,"elapsed_ms":1}

$ curl -s localhost:8080/api/summary | jq '.relations'
{ "total": 60, "resolved": 60 }
```

---

## 구조

```
proto/collector.proto          서비스 정의
cmd/server                     수집 서버 (gRPC + HTTP + 대시보드 서빙)
cmd/agent                      수집 에이전트
internal/collector             client-go 수집 — 메타 · 메트릭 · 로그
internal/server                gRPC 서비스 구현 + 배치 검증
internal/store                 메모리 저장소 + 이름→UID 해석
internal/api                   조회 HTTP/JSON API
web/                           Vue 3 대시보드 (Vite)
deploy/sample-workloads.yaml   수집 대상 샘플
```

---

## 범위와 한계

- **저장소는 메모리다.** 실제 제품이라면 적재기가 Kafka 를 소비해
  MariaDB · OpenSearch · Prometheus 로 라우팅한다. 여기서는 전송 계층과
  수집 정확성을 보는 것이 목적이라 저장은 메모리로 한정했다.
- **인증(mTLS)과 재시도 백오프는 없다.**
- **부하 측정을 하지 않았다.** 처리량 수치를 제시할 수 없다.
- 수집 주기가 겹칠 때의 중복 수집 방지(single-flight)는 넣지 않았다.
- 메트릭은 metrics-server 에 의존한다. 설치되지 않은 클러스터에서는
  메타와 로그만 수집하고 경고를 남긴 뒤 계속 진행한다.
