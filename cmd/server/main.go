package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-metric-collector/gen/collector/v1"
	"github.com/pxvnc1617/grpc-metric-collector/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

func main() {
	addr := flag.String("addr", ":50051", "listen address")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen failed", "addr", *addr, "err", err)
		os.Exit(1)
	}

	// 장시간 유지되는 스트림이 중간 장비에 의해 끊기지 않도록 keepalive 를 켠다.
	// 수집 에이전트는 유휴 구간이 길어질 수 있어 이 설정이 없으면 재연결이 잦아진다.
	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.MaxRecvMsgSize(16*1024*1024), // 배치 전송을 고려해 기본 4MB 에서 상향
	)

	collectorv1.RegisterCollectorServiceServer(grpcServer, server.New(log))
	reflection.Register(grpcServer) // grpcurl 로 확인할 수 있도록

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		grpcServer.GracefulStop() // 진행 중인 스트림을 끊지 않고 마무리
	}()

	log.Info("server started", "addr", *addr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
