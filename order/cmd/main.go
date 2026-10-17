package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	orderHandler "github.com/aidar/order/pkg/handler"
	inventoryv1 "github.com/aidar/shared/pkg/proto/inventory/v1"
	paymentv1 "github.com/aidar/shared/pkg/proto/payment/v1"
)

const (
	inventoryServiceAddress = "localhost:50051"
	paymentServiceAddress   = "localhost:50052"
	httpAddress             = ":8080"

	// Таймауты HTTP сервера
	readHeaderTimeout = 5 * time.Second // защита от Slowloris
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Параметры keepalive gRPC клиентов.
// grpcClientKeepaliveTime не меньше MinTime серверов (5 минут), иначе сервер закроет
// соединение с ENHANCE_YOUR_CALM.
const (
	grpcClientKeepaliveTime    = 5 * time.Minute
	grpcClientKeepaliveTimeout = 3 * time.Second
)

var clientKeepalive = keepalive.ClientParameters{
	Time:                grpcClientKeepaliveTime,
	Timeout:             grpcClientKeepaliveTimeout,
	PermitWithoutStream: true,
}

func main() {
	if run() != nil {
		os.Exit(1)
	}
}

func run() error {
	// Создать gRPC соединение с InventoryService
	inventoryConn, err := grpc.NewClient(
		inventoryServiceAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(clientKeepalive),
	)
	if err != nil {
		slog.Error("не удалось подключиться к InventoryService", "error", err)
		return err
	}
	defer inventoryConn.Close()

	// Создать gRPC соединение с PaymentService
	paymentConn, err := grpc.NewClient(
		paymentServiceAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(clientKeepalive),
	)
	if err != nil {
		slog.Error("не удалось подключиться к PaymentService", "error", err)
		return err
	}
	defer paymentConn.Close()

	// Создаём хранилище и обработчик
	store := orderHandler.NewOrderStore()
	h := orderHandler.NewHandler(
		inventoryv1.NewInventoryServiceClient(inventoryConn),
		paymentv1.NewPaymentServiceClient(paymentConn),
		store,
	)

	// Создать OpenAPI сервер
	orderServer, err := orderHandler.SetupServer(h)
	if err != nil {
		slog.Error("ошибка создания сервера OpenAPI", "error", err)
		return err
	}

	httpServer := &http.Server{
		Addr:              httpAddress,
		Handler:           orderServer,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("запуск OrderService", "port", 8080)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err = <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("ошибка запуска сервера", "error", err)
			return err
		}

		return nil
	case <-ctx.Done():
	}

	slog.Info("получен сигнал завершения, останавливаем OrderService")

	// Shutdown перестаёт принимать новые соединения и ждёт завершения текущих запросов.
	// gRPC соединения закрываются через defer после выхода из main.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err = httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("ошибка graceful shutdown", "error", err)
		return err
	}

	slog.Info("OrderService остановлен")

	return nil
}
