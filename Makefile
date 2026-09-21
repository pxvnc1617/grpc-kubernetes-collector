.PHONY: tools proto build test lint web run-server run-agent cluster clean

PROTO_DIR := proto
MODULE    := github.com/pxvnc1617/grpc-kubernetes-collector
CLUSTER   := grpc-k8s-collector

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
	kind create cluster --name $(CLUSTER) --image kindest/node:v1.29.4
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

## clean: 빌드 산출물과 클러스터 제거
clean:
	rm -rf bin coverage.out web/dist
	-kind delete cluster --name $(CLUSTER)
