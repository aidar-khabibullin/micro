package service

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	paymentv1 "github.com/aidar/shared/pkg/proto/payment/v1"
)

// server реализует gRPC сервис оплаты
type server struct {
	paymentv1.UnimplementedPaymentServiceServer
}

// NewServer создаёт новый экземпляр сервера оплаты
func NewServer() *server {
	return &server{}
}

// PayOrder обрабатывает оплату заказа
func (s *server) PayOrder(
	ctx context.Context,
	req *paymentv1.PayOrderRequest,
) (*paymentv1.PayOrderResponse, error) {
	// TODO: Реализовать метод
	// 1. Проверить, что order_uuid не пустой → INVALID_ARGUMENT
	// 2. Проверить, что payment_method != UNSPECIFIED → INVALID_ARGUMENT
	// 3. Проверить формат UUID → INVALID_ARGUMENT
	// 4. Сгенерировать transaction_uuid (UUID v4)
	// 5. Вывести в лог: "оплата прошла успешно, order_uuid: X, transaction_uuid: Y"
	// 6. Вернуть transaction_uuid

	slog.Info("оплата прошла успешно",
		"order_uuid", req.GetOrderUuid(),
	)

	return nil, status.Error(codes.Unimplemented, "метод PayOrder не реализован")
}
