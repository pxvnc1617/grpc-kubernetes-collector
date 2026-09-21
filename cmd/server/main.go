package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	collectorv1 "github.com/pxvnc1617/grpc-kubernetes-collector/gen/collector/v1"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/api"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/server"
	"github.com/pxvnc1617/grpc-kubernetes-collector/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

func main() {
	grpcAddr := flag.String("grpc", ":50051", "gRPC listen address")
	httpAddr := flag.String("http", ":8080", "HTTP listen address")
	webDir := flag.String("web", "web/dist", "static web directory (built frontend)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	st := store.New()

	// ── gRPC: 수집 결과 수신 ──────────────────────────────────
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Error("grpc listen failed", "addr", *grpcAddr, "err", err)
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
	collectorv1.RegisterCollectorServiceServer(grpcServer, server.New(log, st))
	reflection.Register(grpcServer) // grpcurl 로 확인할 수 있도록

	// ── HTTP: 조회 API + 대시보드 ─────────────────────────────
	mux := api.New(st).Routes()
	mux.Handle("/", spaHandler(*webDir))

	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		grpcServer.GracefulStop() // 진행 중인 스트림을 끊지 않고 마무리
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutCtx)
	}()

	go func() {
		log.Info("http server started", "addr", *httpAddr, "web", *webDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http serve failed", "err", err)
		}
	}()

	log.Info("grpc server started", "addr", *grpcAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Error("grpc serve failed", "err", err)
		os.Exit(1)
	}
}

// spaHandler 는 빌드된 프론트엔드를 서빙한다.
// 파일이 없으면 index.html 로 돌려 SPA 라우팅이 깨지지 않게 한다.
func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			http.Error(w, "프론트엔드가 빌드되지 않았습니다. web/ 에서 `npm run build` 를 실행하세요.", http.StatusNotFound)
			return
		}
		path := dir + r.URL.Path
		if fi, err := os.Stat(path); err != nil || fi.IsDir() {
			if r.URL.Path != "/" {
				http.ServeFile(w, r, dir+"/index.html")
				return
			}
		}
		fs.ServeHTTP(w, r)
	})
}
