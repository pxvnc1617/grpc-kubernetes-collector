.PHONY: tools proto build test lint web run-server run-agent cluster image deploy undeploy logs clean

PROTO_DIR := proto
MODULE    := github.com/pxvnc1617/grpc-kubernetes-collector
CLUSTER   := grpc-k8s-collector

# 이미지 태그를 커밋 해시로 고정한다.
#
# :dev 처럼 바뀌는 태그를 쓰면, 같은 태그로 다시 올렸을 때 노드의 containerd 가
# 옛 이미지를 그대로 쓰는 일이 생긴다. "배포했는데 옛 코드가 돈다" 가 여기서 나온다.
# 태그가 매번 달라지면 그런 모호함이 없다.
IMAGE     := grpc-kubernetes-collector
TAG       := $(shell git rev-parse --short HEAD)$(shell git diff --quiet || echo -dirty)

## tools: protoc 플러그인 설치 (protoc 자체는 별도 설치 필요)
tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	go install sigs.k8s.io/kind@latest

## proto: .proto 에서 Go 코드 생성 → gen/collector/v1/
proto:
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO_DIR)/collector.proto

## cluster: 로컬 kind 클러스터 + metrics-server + 샘플 워크로드
cluster:
	kind create cluster --name $(CLUSTER) --config deploy/kind-cluster.yaml
	kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.1/components.yaml
	kubectl patch deployment metrics-server -n kube-system --type=json \
		-p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
	kubectl apply -f deploy/sample-workloads.yaml
	kubectl wait --for=condition=available --timeout=180s deployment --all -n shop
	kubectl wait --for=condition=available --timeout=120s deployment --all -n ops

## web: 프론트엔드 빌드 → web/dist
web:
	cd web && npm install && npm run build

## build: 서버·에이전트 바이너리 빌드
build: web
	go build -o bin/server ./cmd/server
	go build -o bin/agent  ./cmd/agent
	go build -o bin/node-agent ./cmd/node-agent

## test: 전체 테스트 + 커버리지
test:
	go test ./... -race -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

## lint: 포맷 검사 + vet
lint:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt 위반:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

## run-server: 수집 서버 + 대시보드 (gRPC :50051, HTTP :8080)
run-server:
	go run ./cmd/server

## run-agent: 수집 에이전트 (20초 주기)
run-agent:
	go run ./cmd/agent

## image: 이미지를 빌드해 kind 노드에 적재 (태그 = 커밋 해시)
image:
	docker build -t $(IMAGE):$(TAG) .
	kind load docker-image $(IMAGE):$(TAG) --name $(CLUSTER)

## deploy: 클러스터에 배포 (수집기가 자기 클러스터를 수집한다)
deploy: image
	kubectl apply -f deploy/k8s/
	kubectl -n collector set image deployment/collector-server server=$(IMAGE):$(TAG)
	kubectl -n collector set image deployment/collector-agent  agent=$(IMAGE):$(TAG)
	kubectl -n collector set image daemonset/collector-node-agent node-agent=$(IMAGE):$(TAG)
	kubectl -n collector rollout status deployment/collector-server --timeout=180s
	kubectl -n collector rollout status deployment/collector-agent  --timeout=180s
	kubectl -n collector rollout status daemonset/collector-node-agent --timeout=180s
	@echo "배포된 태그: $(TAG)"
	@echo
	@echo "대시보드: http://localhost:30080  (extraPortMappings 로 만든 클러스터)"
	@echo "그 외   : kubectl -n collector port-forward svc/collector-server 8080:8080"

## undeploy: 배포 제거
undeploy:
	kubectl delete -f deploy/k8s/ --ignore-not-found

## logs: 에이전트 로그 추적
logs:
	kubectl -n collector logs -f deployment/collector-agent

## clean: 빌드 산출물과 클러스터 제거
clean:
	rm -rf bin coverage.out web/dist
	-kind delete cluster --name $(CLUSTER)
