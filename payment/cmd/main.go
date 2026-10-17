package main

import (
	"log/slog"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	paymentService "github.com/aidar/payment/pkg/service"
	paymentv1 "github.com/aidar/shared/pkg/proto/payment/v1"
)

const grpcAddress = ":50052"

func main() {
	lis, err := net.Listen("tcp", grpcAddress)
	if err != nil {
		slog.Error("не удалось создать listener", "error", err)
		os.Exit(1)
	}

	// TODO: Настроить gRPC сервер с параметрами keepalive
	// Подумайте, какие параметры стоит задать для production-ready сервера
	// См. examples/week_1/GRPC_CONNECTIONS.md
	grpcServer := grpc.NewServer()
	paymentv1.RegisterPaymentServiceServer(grpcServer, paymentService.NewServer())

	// Включаем reflection для postman/grpcurl
	reflection.Register(grpcServer)

	slog.Info("запуск PaymentService", "адрес", grpcAddress)

	// TODO: Реализовать graceful shutdown
	// При получении сигнала SIGINT/SIGTERM сервер должен:
	// 1. Перестать принимать новые соединения
	// 2. Дождаться завершения текущих запросов
	// 3. Корректно завершить работу
	// Подсказка: используйте signal.NotifyContext и grpcServer.GracefulStop()

	err = grpcServer.Serve(lis)
	if err != nil {
		slog.Error("ошибка запуска сервера", "error", err)
		os.Exit(1)
	}
}
