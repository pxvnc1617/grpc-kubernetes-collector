# grpc-metric-collector

gRPC 기반 메트릭 수집 파이프라인. 수집 에이전트와 수집 서버 사이의
전송 계층을 gRPC 스트리밍으로 구현했다.

실무에서 HTTP 기반 수집 에이전트를 개발하며 겪었던 문제들 — 배치 전송,
수집 대상 변경, 부분 실패 처리 — 을 gRPC로 다시 풀어본 프로젝트다.

---

## 설계 의도

### 왜 client streaming인가

수집 결과를 건건이 전송하면 RPC 왕복 비용이 처리량을 지배한다.
에이전트가 메트릭을 배치로 묶어 하나의 스트림에 연속 전송하고,
서버는 스트림이 끝날 때 집계 결과를 한 번 돌려준다.

```
Report(stream MetricBatch) returns (ReportSummary)
```

### 왜 수집 대상을 서버가 푸시하는가

수집 범위가 바뀔 때마다 에이전트를 재배포하면 운영 비용이 크다.
에이전트는 자신이 수집 **가능한** 대상만 알리고, 실제로 무엇을
수집할지는 서버가 결정해 스트림으로 내려준다.

```
Subscribe(SubscribeRequest) returns (stream CollectTarget)
```

### 부분 실패가 스트림 전체를 죽이지 않는다

검증에 실패한 배치는 버리고 로그를 남기되, 스트림은 계속 유지한다.
수집기는 일부 대상이 실패해도 나머지가 계속 흘러야 하기 때문이다.
버린 개수는 `ReportSummary.rejected`로 돌려주어 호출자가 인지할 수 있게 했다.

### keepalive

수집 에이전트는 유휴 구간이 길어질 수 있다. keepalive 없이 두면
중간 장비가 연결을 끊어 재연결이 잦아지므로 서버에서 명시적으로 설정했다.

---

## 구조

```
proto/collector.proto        서비스 정의 (unary / client stream / server stream)
cmd/server                   수집 서버
cmd/agent                    수집 에이전트 (클라이언트)
internal/server              서비스 구현 + 배치 검증
```

---

## 실행

### 사전 준비

```bash
# protoc 설치 (Windows: winget install protobuf / macOS: brew install protobuf)
make tools          # protoc-gen-go, protoc-gen-go-grpc 설치
make proto          # .proto → Go 코드 생성
```

### 로컬 실행

```bash
make run-server     # 터미널 1
make run-agent      # 터미널 2
```

에이전트는 헬스체크 → 수집 대상 구독 → 배치 20개(각 100건) 전송 후
집계 결과를 출력한다.

```
level=INFO msg="report summary" batches=20 metrics=2000 rejected=0 elapsed_ms=18
```

### 테스트

```bash
make test           # bufconn 기반. 실제 포트 없이 gRPC 스택을 태운다
```

### 컨테이너

```bash
make docker
docker run --rm -p 50051:50051 grpc-metric-collector:local
```

---

## CI

GitHub Actions에서 다음을 검증한다.

| 단계 | 내용 |
|---|---|
| Lint | `gofmt` 위반 검사, `go vet` |
| Test | `-race -cover` |
| Build | Docker 이미지 빌드 후 기동 확인 |

---

## 확인한 것

- gRPC 세 가지 통신 패턴 (unary / client streaming / server streaming)
- 스트림 수명주기와 `io.EOF` 처리, `CloseAndRecv` / `SendAndClose`
- keepalive, `MaxRecvMsgSize` 등 장시간 스트림을 위한 서버 옵션
- `GracefulStop`으로 진행 중인 스트림을 끊지 않는 종료
- bufconn을 이용한 네트워크 없는 gRPC 통합 테스트
