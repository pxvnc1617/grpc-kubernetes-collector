.PHONY: tools proto build test lint run-server run-agent docker clean

PROTO_DIR := proto
MODULE    := github.com/pxvnc1617/grpc-metric-collector

## tools: protoc 플러그인 설치 (protoc 자체는 별도 설치 필요)
tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

## proto: .proto 에서 Go 코드 생성 → gen/collector/v1/
proto:
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO_DIR)/collector.proto

## build: 서버·에이전트 바이너리 빌드
build:
	go build -o bin/server ./cmd/server
	go build -o bin/agent  ./cmd/agent

## test: 테스트 실행 (커버리지 포함)
test:
	go test ./... -race -cover

## lint: 정적 분석
lint:
	go vet ./...
	gofmt -l .

## run-server: 수집 서버 기동
run-server:
	go run ./cmd/server

## run-agent: 에이전트로 메트릭 전송
run-agent:
	go run ./cmd/agent -batches 20 -per-batch 100

## docker: 이미지 빌드
docker:
	docker build -t grpc-metric-collector:local .

clean:
	rm -rf bin
