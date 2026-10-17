package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	inventoryService "github.com/aidar/inventory/pkg/service"
	inventoryv1 "github.com/aidar/shared/pkg/proto/inventory/v1"
)

const (
	grpcAddress     = ":50051"
	shutdownTimeout = 10 * time.Second
)

// Параметры keepalive gRPC сервера
const (
	grpcMaxConnectionIdle     = 15 * time.Minute
	grpcMaxConnectionAge      = 30 * time.Minute
	grpcMaxConnectionAgeGrace = 5 * time.Second
	grpcKeepaliveTime         = 5 * time.Minute
	grpcKeepaliveTimeout      = 1 * time.Second
	grpcMinPingInterval       = 5 * time.Minute
)

func main() {
	if run() != nil {
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig

	lis, err := lc.Listen(ctx, "tcp", grpcAddress)
	if err != nil {
		slog.Error("не удалось создать listener", "error", err)
		return err
	}

	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Закрываем соединения без активных RPC, чтобы не копились "забытые" коннекты
			MaxConnectionIdle: grpcMaxConnectionIdle,
			// Принудительная ротация соединений для равномерной балансировки между инстансами
			MaxConnectionAge: grpcMaxConnectionAge,
			// Время на завершение активных RPC после GOAWAY
			MaxConnectionAgeGrace: grpcMaxConnectionAgeGrace,
			// Интервал ping'ов клиенту для обнаружения "мёртвых" соединений
			Time: grpcKeepaliveTime,
			// Сколько ждать ответа на ping
			Timeout: grpcKeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// Минимальный интервал между ping'ами клиента, иначе GOAWAY + ENHANCE_YOUR_CALM
			MinTime: grpcKeepaliveTime,
			// Разрешаем ping'и без активных RPC, чтобы клиенты держали соединение "тёплым"
			PermitWithoutStream: true,
		}),
	)
	inventoryv1.RegisterInventoryServiceServer(grpcServer, inventoryService.NewServer())

	// Включаем reflection для postman/grpcurl
	reflection.Register(grpcServer)

	slog.Info("запуск InventoryService", "адрес", grpcAddress)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcServer.Serve(lis)
	}()

	select {
	case err = <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			slog.Error("ошибка запуска сервера", "error", err)
			return err
		}

		return nil
	case <-ctx.Done():
	}

	slog.Info("получен сигнал завершения, останавливаем InventoryService")

	// GracefulStop перестаёт принимать новые соединения и ждёт завершения текущих запросов
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		slog.Info("InventoryService остановлен")
	case <-time.After(shutdownTimeout):
		slog.Warn("таймаут graceful shutdown, принудительная остановка")
		grpcServer.Stop()
	}

	return nil
}
