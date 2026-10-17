package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/ogenerrors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/aidar/shared/pkg/openapi/order/v1"
	inventoryv1 "github.com/aidar/shared/pkg/proto/inventory/v1"
	paymentv1 "github.com/aidar/shared/pkg/proto/payment/v1"
)

// grpcCallTimeout — таймаут на один вызов в Inventory/Payment сервисы
const grpcCallTimeout = 5 * time.Second

// OrderStatus — статус заказа
type OrderStatus string

const (
	OrderStatusPendingPayment OrderStatus = "PENDING_PAYMENT"
	OrderStatusPaid           OrderStatus = "PAID"
	OrderStatusCancelled      OrderStatus = "CANCELLED"
)

// PaymentMethod — способ оплаты заказа
type PaymentMethod string

const (
	PaymentMethodCard          PaymentMethod = "CARD"
	PaymentMethodSBP           PaymentMethod = "SBP"
	PaymentMethodCreditCard    PaymentMethod = "CREDIT_CARD"
	PaymentMethodInvestorMoney PaymentMethod = "INVESTOR_MONEY"
)

// Order представляет заказ на постройку космического корабля
type Order struct {
	OrderUUID       uuid.UUID
	HullUUID        uuid.UUID
	EngineUUID      uuid.UUID
	ShieldUUID      *uuid.UUID // опциональный
	WeaponUUID      *uuid.UUID // опциональный
	TotalPrice      int64      // в копейках
	TransactionUUID *uuid.UUID
	PaymentMethod   *PaymentMethod
	Status          OrderStatus
	CreatedAt       time.Time
}

// orderStore — хранилище заказов (in-memory)
type orderStore struct {
	mu     sync.RWMutex
	orders map[uuid.UUID]Order
}

// NewOrderStore создаёт новое пустое хранилище заказов
func NewOrderStore() *orderStore {
	return &orderStore{
		orders: make(map[uuid.UUID]Order),
	}
}

// handler реализует интерфейс orderv1.Handler, сгенерированный ogen
type handler struct {
	orderv1.UnimplementedHandler
	inventoryClient inventoryv1.InventoryServiceClient
	paymentClient   paymentv1.PaymentServiceClient
	store           *orderStore
}

// NewHandler создаёт новый обработчик заказов
func NewHandler(
	inventoryClient inventoryv1.InventoryServiceClient,
	paymentClient paymentv1.PaymentServiceClient,
	store *orderStore,
) *handler {
	return &handler{
		inventoryClient: inventoryClient,
		paymentClient:   paymentClient,
		store:           store,
	}
}

// SetupServer создаёт OpenAPI сервер на основе обработчика
func SetupServer(h *handler) (*orderv1.Server, error) {
	return orderv1.NewServer(h, orderv1.WithErrorHandler(errorHandler))
}

// errorHandler формирует ответ на ошибки ogen (разбор параметров и тела запроса, валидация)
// в формате схемы Error: {"code": ..., "message": ...}.
// Технические детали ошибки пишутся в лог, клиенту отдаётся сообщение на русском.
func errorHandler(ctx context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	code := ogenerrors.ErrorCode(err)

	message := "внутренняя ошибка сервера"

	switch code {
	case http.StatusBadRequest:
		message = "некорректный запрос"
	case http.StatusUnsupportedMediaType:
		message = "неподдерживаемый тип содержимого"
	}

	slog.WarnContext(ctx, "ошибка обработки запроса", "code", code, "error", err)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	if encodeErr := json.NewEncoder(w).Encode(orderv1.Error{Code: code, Message: message}); encodeErr != nil {
		slog.ErrorContext(ctx, "не удалось записать ответ с ошибкой", "error", encodeErr)
	}
}

// GetOrder реализует операцию getOrder (пример реализации)
// GET /api/v1/orders/{order_uuid}.
func (h *handler) GetOrder(_ context.Context, params orderv1.GetOrderParams) (orderv1.GetOrderRes, error) {
	// 1. Найти заказ в store (с блокировкой для thread-safety)
	h.store.mu.RLock()
	order, ok := h.store.orders[params.OrderUUID]
	h.store.mu.RUnlock()

	// 2. Если не найден — вернуть 404
	if !ok {
		return &orderv1.GetOrderNotFound{
			Code:    http.StatusNotFound,
			Message: "заказ не найден",
		}, nil
	}

	// 3. Преобразовать в DTO и вернуть
	var shieldUUID orderv1.OptNilUUID
	if order.ShieldUUID != nil {
		shieldUUID = orderv1.NewOptNilUUID(*order.ShieldUUID)
	}

	var weaponUUID orderv1.OptNilUUID
	if order.WeaponUUID != nil {
		weaponUUID = orderv1.NewOptNilUUID(*order.WeaponUUID)
	}

	var transactionUUID orderv1.OptNilUUID
	if order.TransactionUUID != nil {
		transactionUUID = orderv1.NewOptNilUUID(*order.TransactionUUID)
	}

	var paymentMethod orderv1.OptNilPaymentMethod
	if order.PaymentMethod != nil {
		paymentMethod = orderv1.NewOptNilPaymentMethod(orderv1.PaymentMethod(*order.PaymentMethod))
	}

	return &orderv1.OrderDto{
		OrderUUID:       order.OrderUUID,
		HullUUID:        order.HullUUID,
		EngineUUID:      order.EngineUUID,
		ShieldUUID:      shieldUUID,
		WeaponUUID:      weaponUUID,
		TotalPrice:      order.TotalPrice,
		TransactionUUID: transactionUUID,
		PaymentMethod:   paymentMethod,
		Status:          orderv1.OrderStatus(order.Status),
		CreatedAt:       order.CreatedAt,
	}, nil
}

// CreateOrder реализует операцию createOrder
// POST /api/v1/orders.
func (h *handler) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (orderv1.CreateOrderRes, error) {
	// 1. Валидация: hull_uuid и engine_uuid обязательны
	if req.HullUUID == uuid.Nil || req.EngineUUID == uuid.Nil {
		return &orderv1.CreateOrderBadRequest{
			Code:    http.StatusBadRequest,
			Message: "hull_uuid и engine_uuid обязательны",
		}, nil
	}

	shieldUUID := optUUIDToPtr(req.ShieldUUID)
	weaponUUID := optUUIDToPtr(req.WeaponUUID)

	partUUIDs := []string{req.HullUUID.String(), req.EngineUUID.String()}
	if shieldUUID != nil {
		partUUIDs = append(partUUIDs, shieldUUID.String())
	}
	if weaponUUID != nil {
		partUUIDs = append(partUUIDs, weaponUUID.String())
	}

	// 2. Получить детали через InventoryService.ListParts
	callCtx, cancel := context.WithTimeout(ctx, grpcCallTimeout)
	defer cancel()

	resp, err := h.inventoryClient.ListParts(callCtx, &inventoryv1.ListPartsRequest{Uuids: partUUIDs})
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound:
			return &orderv1.CreateOrderNotFound{Code: http.StatusNotFound, Message: "компонент не найден"}, nil
		case codes.InvalidArgument:
			return &orderv1.CreateOrderBadRequest{Code: http.StatusBadRequest, Message: "неверный формат uuid компонента"}, nil
		default:
			slog.ErrorContext(ctx, "не удалось получить компоненты со склада", "error", err)

			return &orderv1.CreateOrderInternalServerError{
				Code:    http.StatusInternalServerError,
				Message: "не удалось получить компоненты со склада",
			}, nil
		}
	}

	if len(resp.GetParts()) != len(partUUIDs) {
		return &orderv1.CreateOrderNotFound{Code: http.StatusNotFound, Message: "компонент не найден"}, nil
	}

	// 3. Проверить stock_quantity > 0
	// 4. Вычислить total_price
	var totalPrice int64
	for _, part := range resp.GetParts() {
		if part.GetStockQuantity() <= 0 {
			return &orderv1.CreateOrderConflict{
				Code:    http.StatusConflict,
				Message: "компонент отсутствует на складе: " + part.GetName(),
			}, nil
		}

		totalPrice += part.GetPrice()
	}

	// 5. Сгенерировать order_uuid (UUID v4)
	orderUUID := uuid.New()

	// 6. Создать заказ со статусом PENDING_PAYMENT
	order := Order{
		OrderUUID:  orderUUID,
		HullUUID:   req.HullUUID,
		EngineUUID: req.EngineUUID,
		ShieldUUID: shieldUUID,
		WeaponUUID: weaponUUID,
		TotalPrice: totalPrice,
		Status:     OrderStatusPendingPayment,
		CreatedAt:  time.Now(),
	}

	// 7. Сохранить в store
	h.store.mu.Lock()
	h.store.orders[orderUUID] = order
	h.store.mu.Unlock()

	// 8. Вернуть order_uuid и total_price
	return &orderv1.CreateOrderResponse{
		OrderUUID:  orderUUID,
		TotalPrice: totalPrice,
	}, nil
}

// PayOrder реализует операцию payOrder
// POST /api/v1/orders/{order_uuid}/pay.
func (h *handler) PayOrder(
	ctx context.Context,
	req *orderv1.PayOrderRequest,
	params orderv1.PayOrderParams,
) (orderv1.PayOrderRes, error) {
	// 1. Найти заказ в store
	h.store.mu.RLock()
	order, ok := h.store.orders[params.OrderUUID]
	h.store.mu.RUnlock()

	if !ok {
		return &orderv1.PayOrderNotFound{Code: http.StatusNotFound, Message: "заказ не найден"}, nil
	}

	// 2. Проверить статус == PENDING_PAYMENT
	if order.Status != OrderStatusPendingPayment {
		return &orderv1.PayOrderConflict{
			Code:    http.StatusConflict,
			Message: "заказ нельзя оплатить в статусе " + string(order.Status),
		}, nil
	}

	// 3. Вызвать h.paymentClient.PayOrder для обработки платежа
	callCtx, cancel := context.WithTimeout(ctx, grpcCallTimeout)
	defer cancel()

	payResp, err := h.paymentClient.PayOrder(callCtx, &paymentv1.PayOrderRequest{
		OrderUuid:     order.OrderUUID.String(),
		PaymentMethod: toProtoPaymentMethod(req.PaymentMethod),
	})
	if err != nil {
		if status.Code(err) == codes.InvalidArgument {
			return &orderv1.PayOrderBadRequest{Code: http.StatusBadRequest, Message: status.Convert(err).Message()}, nil
		}

		slog.ErrorContext(ctx, "не удалось провести оплату", "order_uuid", order.OrderUUID, "error", err)

		return &orderv1.PayOrderInternalServerError{
			Code:    http.StatusInternalServerError,
			Message: "не удалось провести оплату",
		}, nil
	}

	transactionUUID, err := uuid.Parse(payResp.GetTransactionUuid())
	if err != nil {
		return &orderv1.PayOrderInternalServerError{
			Code:    http.StatusInternalServerError,
			Message: "сервис оплаты вернул неверный transaction_uuid",
		}, nil
	}

	// 4. Обновить статус на PAID и сохранить transaction_uuid
	paymentMethod := PaymentMethod(req.PaymentMethod)

	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	// Пока шёл вызов в PaymentService, заказ могли отменить или оплатить параллельно
	order = h.store.orders[params.OrderUUID]
	if order.Status != OrderStatusPendingPayment {
		return &orderv1.PayOrderConflict{
			Code:    http.StatusConflict,
			Message: "заказ нельзя оплатить в статусе " + string(order.Status),
		}, nil
	}

	order.Status = OrderStatusPaid
	order.TransactionUUID = &transactionUUID
	order.PaymentMethod = &paymentMethod
	h.store.orders[params.OrderUUID] = order

	// 5. Вернуть transaction_uuid
	return &orderv1.PayOrderResponse{TransactionUUID: transactionUUID}, nil
}

// CancelOrder реализует операцию cancelOrder
// POST /api/v1/orders/{order_uuid}/cancel.
func (h *handler) CancelOrder(_ context.Context, params orderv1.CancelOrderParams) (orderv1.CancelOrderRes, error) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	// 1. Найти заказ в store
	order, ok := h.store.orders[params.OrderUUID]
	if !ok {
		return &orderv1.CancelOrderNotFound{Code: http.StatusNotFound, Message: "заказ не найден"}, nil
	}

	// 2. Проверить статус == PENDING_PAYMENT
	if order.Status != OrderStatusPendingPayment {
		return &orderv1.CancelOrderConflict{
			Code:    http.StatusConflict,
			Message: "заказ нельзя отменить в статусе " + string(order.Status),
		}, nil
	}

	// 3. Обновить статус на CANCELLED
	order.Status = OrderStatusCancelled
	h.store.orders[params.OrderUUID] = order

	// 4. Вернуть success
	return &orderv1.CancelOrderResponse{}, nil
}

// optUUIDToPtr преобразует опциональный nullable UUID в указатель (nil, если значение не задано)
func optUUIDToPtr(v orderv1.OptNilUUID) *uuid.UUID {
	id, ok := v.Get()
	if !ok {
		return nil
	}

	return &id
}

// toProtoPaymentMethod преобразует способ оплаты из OpenAPI в gRPC enum
func toProtoPaymentMethod(m orderv1.PaymentMethod) paymentv1.PaymentMethod {
	switch m {
	case orderv1.PaymentMethodCARD:
		return paymentv1.PaymentMethod_PAYMENT_METHOD_CARD
	case orderv1.PaymentMethodSBP:
		return paymentv1.PaymentMethod_PAYMENT_METHOD_SBP
	case orderv1.PaymentMethodCREDITCARD:
		return paymentv1.PaymentMethod_PAYMENT_METHOD_CREDIT_CARD
	case orderv1.PaymentMethodINVESTORMONEY:
		return paymentv1.PaymentMethod_PAYMENT_METHOD_INVESTOR_MONEY
	default:
		return paymentv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED
	}
}
