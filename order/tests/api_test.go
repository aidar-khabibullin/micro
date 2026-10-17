// Package tests содержит API-тесты недели 1.
//
// Схема: всё поднимается in-process в одном бинарнике теста.
//   - Inventory и Payment (gRPC) поднимаются через bufconn.Listener
//     (виртуальный транспорт без сети), клиенты ходят к ним через
//     grpc.NewClient с custom dialer'ом
//   - Order (HTTP) поднимается через httptest.NewServer (реальный TCP на
//     127.0.0.1 со случайным портом), Order внутри ходит в Inventory/Payment
//     по тем же bufconn-клиентам
//
// Такой стек запускается один раз в TestMain, нет ни Docker, ни хардкод-портов,
// ни task up. Тесты быстрые и самодостаточные.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	inventoryService "github.com/aidar/inventory/pkg/service"
	orderHandler "github.com/aidar/order/pkg/handler"
	"github.com/aidar/order/tests/testutil"
	paymentService "github.com/aidar/payment/pkg/service"
	inventoryv1 "github.com/aidar/shared/pkg/proto/inventory/v1"
	paymentv1 "github.com/aidar/shared/pkg/proto/payment/v1"
)

// Предзагруженные UUID и цены деталей (из inventory/cmd/main.go)
const (
	HullAluminumUUID   = "550e8400-e29b-41d4-a716-446655440001" // 500000 kopecks (5000 RUB)
	HullTitaniumUUID   = "550e8400-e29b-41d4-a716-446655440002" // 1500000 kopecks (15000 RUB)
	EngineIonCUUID     = "550e8400-e29b-41d4-a716-446655440003" // 300000 kopecks (3000 RUB)
	EngineIonBUUID     = "550e8400-e29b-41d4-a716-446655440004" // 800000 kopecks (8000 RUB)
	ShieldEnergyUUID   = "550e8400-e29b-41d4-a716-446655440005" // 400000 kopecks (4000 RUB)
	WeaponLaserUUID    = "550e8400-e29b-41d4-a716-446655440006" // 250000 kopecks (2500 RUB)
	HullOutOfStockUUID = "550e8400-e29b-41d4-a716-446655440007" // 2000000 kopecks (20000 RUB), stock=0

	// Цены в копейках
	HullAluminumPrice   = 500000
	HullTitaniumPrice   = 1500000
	EngineIonCPrice     = 300000
	EngineIonBPrice     = 800000
	ShieldEnergyPrice   = 400000
	WeaponLaserPrice    = 250000
	HullOutOfStockPrice = 2000000
)

const bufSize = 1024 * 1024

var (
	invLis *bufconn.Listener
	payLis *bufconn.Listener

	inventoryClient inventoryv1.InventoryServiceClient
	paymentClient   paymentv1.PaymentServiceClient
	httpClient      = &http.Client{Timeout: 5 * time.Second}
	ts              *httptest.Server
)

func invBufDialer(context.Context, string) (net.Conn, error) {
	return invLis.Dial()
}

func payBufDialer(context.Context, string) (net.Conn, error) {
	return payLis.Dial()
}

// orderBaseURL возвращает базовый URL для HTTP тестов заказов
func orderBaseURL() string {
	return ts.URL
}

// TestMain запускает все сервисы перед тестами и останавливает после
func TestMain(m *testing.M) {
	// 1. Inventory gRPC через bufconn
	invLis = bufconn.Listen(bufSize)
	invGRPCServer := grpc.NewServer()
	inventoryv1.RegisterInventoryServiceServer(invGRPCServer, inventoryService.NewServer())
	go func() {
		if invServeErr := invGRPCServer.Serve(invLis); invServeErr != nil {
			slog.Error("inventory gRPC server failed", "error", invServeErr)
			os.Exit(1)
		}
	}()

	invConn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(invBufDialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		slog.Error("inventory gRPC client failed", "error", err)
		os.Exit(1)
	}
	inventoryClient = inventoryv1.NewInventoryServiceClient(invConn)

	// 2. Payment gRPC через bufconn
	payLis = bufconn.Listen(bufSize)
	payGRPCServer := grpc.NewServer()
	paymentv1.RegisterPaymentServiceServer(payGRPCServer, paymentService.NewServer())
	go func() {
		if payServeErr := payGRPCServer.Serve(payLis); payServeErr != nil {
			slog.Error("payment gRPC server failed", "error", payServeErr)
			os.Exit(1)
		}
	}()

	payConn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(payBufDialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		slog.Error("payment gRPC client failed", "error", err)
		os.Exit(1)
	}
	paymentClient = paymentv1.NewPaymentServiceClient(payConn)

	// 3. Order HTTP через httptest
	store := orderHandler.NewOrderStore()
	h := orderHandler.NewHandler(inventoryClient, paymentClient, store)
	orderServer, err := orderHandler.SetupServer(h)
	if err != nil {
		slog.Error("order HTTP server setup failed", "error", err)
		os.Exit(1)
	}
	ts = httptest.NewServer(orderServer)

	code := m.Run()

	ts.Close()
	invConn.Close()
	payConn.Close()
	invGRPCServer.Stop()
	payGRPCServer.Stop()
	os.Exit(code)
}

// HTTP типы запросов/ответов

// CreateOrderRequest представляет тело запроса для создания заказа
type CreateOrderRequest struct {
	HullUUID   string  `json:"hull_uuid"`
	EngineUUID string  `json:"engine_uuid"`
	ShieldUUID *string `json:"shield_uuid,omitempty"`
	WeaponUUID *string `json:"weapon_uuid,omitempty"`
}

// CreateOrderResponse представляет ответ на создание заказа
type CreateOrderResponse struct {
	OrderUUID  string `json:"order_uuid"`
	TotalPrice int64  `json:"total_price"`
}

// PayOrderRequest представляет тело запроса для оплаты заказа
type PayOrderRequest struct {
	PaymentMethod string `json:"payment_method"`
}

// PayOrderResponse представляет ответ на оплату заказа
type PayOrderResponse struct {
	TransactionUUID string `json:"transaction_uuid"`
}

// CancelOrderResponse представляет ответ на отмену заказа (пустой)
type CancelOrderResponse struct{}

// OrderDTO представляет заказ в ответе API
type OrderDTO struct {
	OrderUUID       string  `json:"order_uuid"`
	HullUUID        string  `json:"hull_uuid"`
	EngineUUID      string  `json:"engine_uuid"`
	ShieldUUID      *string `json:"shield_uuid"`
	WeaponUUID      *string `json:"weapon_uuid"`
	TotalPrice      int64   `json:"total_price"`
	TransactionUUID *string `json:"transaction_uuid"`
	PaymentMethod   *string `json:"payment_method"`
	Status          string  `json:"status"`
	CreatedAt       string  `json:"created_at"`
}

// ErrorResponse представляет ответ с ошибкой от API
type ErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Вспомогательные HTTP функции

func createOrder(t *testing.T, req *CreateOrderRequest) (*CreateOrderResponse, *http.Response) {
	t.Helper()

	jsonBody, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders", bytes.NewReader(jsonBody))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)

	if resp.StatusCode == http.StatusCreated {
		var result CreateOrderResponse
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)
		return &result, resp
	}

	return nil, resp
}

func getOrder(t *testing.T, orderUUID string) (*OrderDTO, *http.Response) {
	t.Helper()

	resp, err := httpClient.Get(orderBaseURL() + "/api/v1/orders/" + orderUUID)
	require.NoError(t, err)

	if resp.StatusCode == http.StatusOK {
		var result OrderDTO
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)
		return &result, resp
	}

	return nil, resp
}

func payOrder(t *testing.T, orderUUID string, req *PayOrderRequest) (*PayOrderResponse, *http.Response) {
	t.Helper()

	jsonBody, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders/"+orderUUID+"/pay", bytes.NewReader(jsonBody))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)

	if resp.StatusCode == http.StatusOK {
		var result PayOrderResponse
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)
		return &result, resp
	}

	return nil, resp
}

func cancelOrder(t *testing.T, orderUUID string) (*CancelOrderResponse, *http.Response) {
	t.Helper()

	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders/"+orderUUID+"/cancel", nil)
	require.NoError(t, err)

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)

	if resp.StatusCode == http.StatusOK {
		var result CancelOrderResponse
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)
		return &result, resp
	}

	return nil, resp
}

// Тесты InventoryService (gRPC)

func TestInventory_GetPart_Success(t *testing.T) {
	resp, err := inventoryClient.GetPart(context.Background(), &inventoryv1.GetPartRequest{
		Uuid: HullAluminumUUID,
	})
	require.NoError(t, err)

	part := resp.GetPart()
	assert.Equal(t, HullAluminumUUID, part.GetUuid())
	assert.Equal(t, int64(HullAluminumPrice), part.GetPrice())
	assert.Equal(t, inventoryv1.PartType_PART_TYPE_HULL, part.GetPartType())
	assert.NotEmpty(t, part.GetName())
	assert.NotNil(t, part.GetCreatedAt())
}

func TestInventory_GetPart_AllTypes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		uuid     string
		price    int64
		partType inventoryv1.PartType
	}{
		{"Hull Aluminum", HullAluminumUUID, HullAluminumPrice, inventoryv1.PartType_PART_TYPE_HULL},
		{"Hull Titanium", HullTitaniumUUID, HullTitaniumPrice, inventoryv1.PartType_PART_TYPE_HULL},
		{"Engine Ion C", EngineIonCUUID, EngineIonCPrice, inventoryv1.PartType_PART_TYPE_ENGINE},
		{"Engine Ion B", EngineIonBUUID, EngineIonBPrice, inventoryv1.PartType_PART_TYPE_ENGINE},
		{"Shield Energy", ShieldEnergyUUID, ShieldEnergyPrice, inventoryv1.PartType_PART_TYPE_SHIELD},
		{"Weapon Laser", WeaponLaserUUID, WeaponLaserPrice, inventoryv1.PartType_PART_TYPE_WEAPON},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, err := inventoryClient.GetPart(context.Background(), &inventoryv1.GetPartRequest{
				Uuid: tc.uuid,
			})
			require.NoError(t, err)

			part := resp.GetPart()
			assert.Equal(t, tc.uuid, part.GetUuid())
			assert.Equal(t, tc.price, part.GetPrice())
			assert.Equal(t, tc.partType, part.GetPartType())
		})
	}
}

func TestInventory_GetPart_NotFound(t *testing.T) {
	_, err := inventoryClient.GetPart(context.Background(), &inventoryv1.GetPartRequest{
		Uuid: uuid.New().String(),
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.NotFound)
}

func TestInventory_GetPart_EmptyUUID(t *testing.T) {
	_, err := inventoryClient.GetPart(context.Background(), &inventoryv1.GetPartRequest{
		Uuid: "",
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.InvalidArgument)
}

func TestInventory_GetPart_InvalidUUID(t *testing.T) {
	_, err := inventoryClient.GetPart(context.Background(), &inventoryv1.GetPartRequest{
		Uuid: "invalid-uuid-format",
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.InvalidArgument)
}

func TestInventory_ListParts_All(t *testing.T) {
	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		PartType: inventoryv1.PartType_PART_TYPE_UNSPECIFIED,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 7)
}

func TestInventory_ListParts_ByType_Hull(t *testing.T) {
	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		PartType: inventoryv1.PartType_PART_TYPE_HULL,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 3)

	for _, part := range resp.GetParts() {
		assert.Equal(t, inventoryv1.PartType_PART_TYPE_HULL, part.GetPartType())
	}
}

func TestInventory_ListParts_ByType_Engine(t *testing.T) {
	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		PartType: inventoryv1.PartType_PART_TYPE_ENGINE,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 2)

	for _, part := range resp.GetParts() {
		assert.Equal(t, inventoryv1.PartType_PART_TYPE_ENGINE, part.GetPartType())
	}
}

func TestInventory_ListParts_ByType_Shield(t *testing.T) {
	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		PartType: inventoryv1.PartType_PART_TYPE_SHIELD,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 1)
	assert.Equal(t, ShieldEnergyUUID, resp.GetParts()[0].GetUuid())
}

func TestInventory_ListParts_ByType_Weapon(t *testing.T) {
	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		PartType: inventoryv1.PartType_PART_TYPE_WEAPON,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 1)
	assert.Equal(t, WeaponLaserUUID, resp.GetParts()[0].GetUuid())
}

func TestInventory_ListParts_SortedByName(t *testing.T) {
	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		PartType: inventoryv1.PartType_PART_TYPE_UNSPECIFIED,
	})
	require.NoError(t, err)

	parts := resp.GetParts()
	for i := 1; i < len(parts); i++ {
		assert.LessOrEqual(t, parts[i-1].GetName(), parts[i].GetName(),
			"детали должны быть отсортированы по имени в алфавитном порядке")
	}
}

// Тесты ListParts.uuids

func TestInventory_ListParts_ByUuids_Success(t *testing.T) {
	uuids := []string{HullAluminumUUID, EngineIonCUUID, ShieldEnergyUUID}

	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids: uuids,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 3)

	// Проверяем, что вернулись нужные детали
	returnedUUIDs := make([]string, len(resp.GetParts()))
	for i, part := range resp.GetParts() {
		returnedUUIDs[i] = part.GetUuid()
	}
	assert.ElementsMatch(t, uuids, returnedUUIDs)
}

func TestInventory_ListParts_ByUuids_PreservesOrder(t *testing.T) {
	// Запрос в определённом порядке (engine, hull, weapon)
	uuids := []string{EngineIonCUUID, HullAluminumUUID, WeaponLaserUUID}

	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids: uuids,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 3)

	// Проверяем, что порядок сохранён как в запросе
	for i, part := range resp.GetParts() {
		assert.Equal(t, uuids[i], part.GetUuid(),
			"деталь с индексом %d должна соответствовать порядку запрошенных UUID", i)
	}
}

func TestInventory_ListParts_ByUuids_IgnoresPartType(t *testing.T) {
	// Запрос с uuids И part_type (part_type должен быть проигнорирован)
	uuids := []string{HullAluminumUUID, EngineIonCUUID}

	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids:    uuids,
		PartType: inventoryv1.PartType_PART_TYPE_WEAPON, // Должен быть проигнорирован
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 2)

	// Проверяем, что получили Hull и Engine, а не Weapons
	assert.Equal(t, HullAluminumUUID, resp.GetParts()[0].GetUuid())
	assert.Equal(t, EngineIonCUUID, resp.GetParts()[1].GetUuid())
}

func TestInventory_ListParts_ByUuids_NotFound(t *testing.T) {
	// Включаем один несуществующий UUID
	nonExistentUUID := uuid.New().String()
	uuids := []string{HullAluminumUUID, nonExistentUUID, EngineIonCUUID}

	_, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids: uuids,
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.NotFound)
}

func TestInventory_ListParts_ByUuids_InvalidUUID(t *testing.T) {
	uuids := []string{HullAluminumUUID, "invalid-uuid-format"}

	_, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids: uuids,
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.InvalidArgument)
}

func TestInventory_ListParts_ByUuids_SingleUUID(t *testing.T) {
	uuids := []string{WeaponLaserUUID}

	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids: uuids,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 1)
	assert.Equal(t, WeaponLaserUUID, resp.GetParts()[0].GetUuid())
	assert.Equal(t, int64(WeaponLaserPrice), resp.GetParts()[0].GetPrice())
}

func TestInventory_ListParts_ByUuids_AllParts(t *testing.T) {
	// Запрашиваем все 7 деталей по UUID
	uuids := []string{
		HullAluminumUUID, HullTitaniumUUID,
		EngineIonCUUID, EngineIonBUUID,
		ShieldEnergyUUID, WeaponLaserUUID,
		HullOutOfStockUUID,
	}

	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids: uuids,
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 7)

	// Проверяем, что порядок совпадает с порядком запроса
	for i, part := range resp.GetParts() {
		assert.Equal(t, uuids[i], part.GetUuid())
	}
}

// Тесты PaymentService (gRPC)

func TestPayment_PayOrder_Success_Card(t *testing.T) {
	resp, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     uuid.New().String(),
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_CARD,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetTransactionUuid())

	// Проверяем, что UUID транзакции валиден
	_, err = uuid.Parse(resp.GetTransactionUuid())
	assert.NoError(t, err)
}

func TestPayment_PayOrder_Success_SBP(t *testing.T) {
	resp, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     uuid.New().String(),
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_SBP,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetTransactionUuid())
}

func TestPayment_PayOrder_Success_CreditCard(t *testing.T) {
	resp, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     uuid.New().String(),
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_CREDIT_CARD,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetTransactionUuid())
}

func TestPayment_PayOrder_Success_InvestorMoney(t *testing.T) {
	resp, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     uuid.New().String(),
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_INVESTOR_MONEY,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetTransactionUuid())
}

func TestPayment_PayOrder_EmptyOrderUUID(t *testing.T) {
	_, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     "",
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_CARD,
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.InvalidArgument)
}

func TestPayment_PayOrder_UnspecifiedMethod(t *testing.T) {
	_, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     uuid.New().String(),
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED,
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.InvalidArgument)
}

func TestPayment_PayOrder_UniqueTransactions(t *testing.T) {
	orderUUID := uuid.New().String()

	resp1, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     orderUUID,
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_CARD,
	})
	require.NoError(t, err)

	resp2, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     orderUUID,
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_CARD,
	})
	require.NoError(t, err)

	assert.NotEqual(t, resp1.GetTransactionUuid(), resp2.GetTransactionUuid(),
		"каждый платёж должен генерировать уникальный UUID транзакции")
}

// Тесты OrderService (HTTP)

func TestOrder_Create_Success_MinimalParts(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}

	result, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.OrderUUID)
	assert.Equal(t, int64(HullAluminumPrice+EngineIonCPrice), result.TotalPrice)
}

func TestOrder_Create_Success_AllParts(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullTitaniumUUID,
		EngineUUID: EngineIonBUUID,
		ShieldUUID: new(ShieldEnergyUUID),
		WeaponUUID: new(WeaponLaserUUID),
	}

	result, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.OrderUUID)

	expectedTotal := int64(HullTitaniumPrice + EngineIonBPrice + ShieldEnergyPrice + WeaponLaserPrice)
	assert.Equal(t, expectedTotal, result.TotalPrice)
}

func TestOrder_Create_VerifyTotalPrice(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID, // 500000
		EngineUUID: EngineIonCUUID,   // 300000
	}

	result, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, int64(800000), result.TotalPrice, "500000 + 300000 = 800000")
}

func TestOrder_Create_HullNotFound(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   uuid.New().String(),
		EngineUUID: EngineIonCUUID,
	}

	_, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrder_Create_EngineNotFound(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: uuid.New().String(),
	}

	_, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrder_Create_ShieldNotFound(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
		ShieldUUID: new(uuid.New().String()),
	}

	_, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrder_Create_WeaponNotFound(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
		WeaponUUID: new(uuid.New().String()),
	}

	_, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrder_Get_Success(t *testing.T) {
	// Сначала создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Получаем заказ
	order, resp := getOrder(t, createResult.OrderUUID)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, order)
	assert.Equal(t, createResult.OrderUUID, order.OrderUUID)
	assert.Equal(t, HullAluminumUUID, order.HullUUID)
	assert.Equal(t, EngineIonCUUID, order.EngineUUID)
	assert.Equal(t, createResult.TotalPrice, order.TotalPrice)
}

func TestOrder_Get_VerifyStatus_PendingPayment(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Получаем и проверяем статус
	order, resp := getOrder(t, createResult.OrderUUID)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "PENDING_PAYMENT", order.Status)
}

func TestOrder_Get_NotFound(t *testing.T) {
	_, resp := getOrder(t, uuid.New().String())
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrder_Pay_Success_Card(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Оплачиваем заказ
	payReq := &PayOrderRequest{PaymentMethod: "CARD"}
	payResult, resp := payOrder(t, createResult.OrderUUID, payReq)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, payResult)
	assert.NotEmpty(t, payResult.TransactionUUID)
}

func TestOrder_Pay_VerifyStatusChange(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Оплачиваем заказ
	payReq := &PayOrderRequest{PaymentMethod: "CARD"}
	_, payResp := payOrder(t, createResult.OrderUUID, payReq)
	defer payResp.Body.Close()

	// Получаем и проверяем смену статуса на PAID
	order, getResp := getOrder(t, createResult.OrderUUID)
	defer getResp.Body.Close()

	require.Equal(t, http.StatusOK, getResp.StatusCode)
	assert.Equal(t, "PAID", order.Status)
	assert.NotNil(t, order.TransactionUUID)
	assert.NotNil(t, order.PaymentMethod)
	assert.Equal(t, "CARD", *order.PaymentMethod)
}

func TestOrder_Pay_NotFound(t *testing.T) {
	payReq := &PayOrderRequest{PaymentMethod: "CARD"}
	_, resp := payOrder(t, uuid.New().String(), payReq)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrder_Pay_AlreadyPaid(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Оплачиваем заказ в первый раз
	payReq := &PayOrderRequest{PaymentMethod: "CARD"}
	_, payResp1 := payOrder(t, createResult.OrderUUID, payReq)
	defer payResp1.Body.Close()

	// Пытаемся оплатить повторно (должна быть ошибка конфликта)
	_, payResp2 := payOrder(t, createResult.OrderUUID, payReq)
	defer payResp2.Body.Close()

	require.Equal(t, http.StatusConflict, payResp2.StatusCode)
}

func TestOrder_Pay_AlreadyCancelled(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Отменяем заказ
	_, cancelResp := cancelOrder(t, createResult.OrderUUID)
	defer cancelResp.Body.Close()

	// Пытаемся оплатить отменённый заказ (должна быть ошибка конфликта)
	payReq := &PayOrderRequest{PaymentMethod: "CARD"}
	_, payResp := payOrder(t, createResult.OrderUUID, payReq)
	defer payResp.Body.Close()

	require.Equal(t, http.StatusConflict, payResp.StatusCode)
}

func TestOrder_Cancel_Success(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Отменяем заказ
	_, resp := cancelOrder(t, createResult.OrderUUID)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestOrder_Cancel_VerifyStatusChange(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Отменяем заказ
	_, cancelResp := cancelOrder(t, createResult.OrderUUID)
	defer cancelResp.Body.Close()

	// Получаем и проверяем смену статуса на CANCELLED
	order, getResp := getOrder(t, createResult.OrderUUID)
	defer getResp.Body.Close()

	require.Equal(t, http.StatusOK, getResp.StatusCode)
	assert.Equal(t, "CANCELLED", order.Status)
}

func TestOrder_Cancel_NotFound(t *testing.T) {
	_, resp := cancelOrder(t, uuid.New().String())
	defer resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrder_Cancel_AlreadyPaid(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Оплачиваем заказ
	payReq := &PayOrderRequest{PaymentMethod: "CARD"}
	_, payResp := payOrder(t, createResult.OrderUUID, payReq)
	defer payResp.Body.Close()

	// Пытаемся отменить оплаченный заказ (должна быть ошибка конфликта)
	_, cancelResp := cancelOrder(t, createResult.OrderUUID)
	defer cancelResp.Body.Close()

	require.Equal(t, http.StatusConflict, cancelResp.StatusCode)
}

func TestOrder_Cancel_AlreadyCancelled(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Отменяем заказ первый раз
	_, cancelResp1 := cancelOrder(t, createResult.OrderUUID)
	defer cancelResp1.Body.Close()

	// Пытаемся отменить повторно (должна быть ошибка конфликта)
	_, cancelResp2 := cancelOrder(t, createResult.OrderUUID)
	defer cancelResp2.Body.Close()

	require.Equal(t, http.StatusConflict, cancelResp2.StatusCode)
}

// Дополнительные тесты валидации

func TestOrder_Create_WithWeaponOnly(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
		WeaponUUID: new(WeaponLaserUUID),
	}

	result, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, result)
	expectedTotal := int64(HullAluminumPrice + EngineIonCPrice + WeaponLaserPrice)
	assert.Equal(t, expectedTotal, result.TotalPrice)
}

func TestOrder_Pay_AllMethods(t *testing.T) {
	t.Parallel()

	methods := []string{"CARD", "SBP", "CREDIT_CARD", "INVESTOR_MONEY"}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			// Создаём заказ
			createReq := &CreateOrderRequest{
				HullUUID:   HullAluminumUUID,
				EngineUUID: EngineIonCUUID,
			}
			createResult, createResp := createOrder(t, createReq)
			defer createResp.Body.Close()
			require.NotNil(t, createResult)

			// Оплачиваем этим методом
			payReq := &PayOrderRequest{PaymentMethod: method}
			payResult, resp := payOrder(t, createResult.OrderUUID, payReq)
			defer resp.Body.Close()

			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NotNil(t, payResult)
			assert.NotEmpty(t, payResult.TransactionUUID)

			// Проверяем, что метод оплаты сохранён
			order, getResp := getOrder(t, createResult.OrderUUID)
			defer getResp.Body.Close()
			require.NotNil(t, order.PaymentMethod)
			assert.Equal(t, method, *order.PaymentMethod)
		})
	}
}

func TestOrder_Get_WithOptionalParts(t *testing.T) {
	shieldUUID := ShieldEnergyUUID
	weaponUUID := WeaponLaserUUID
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
		ShieldUUID: &shieldUUID,
		WeaponUUID: &weaponUUID,
	}

	createResult, createResp := createOrder(t, req)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Получаем заказ и проверяем, что опциональные детали сохранены
	order, resp := getOrder(t, createResult.OrderUUID)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, order.ShieldUUID)
	require.NotNil(t, order.WeaponUUID)
	assert.Equal(t, shieldUUID, *order.ShieldUUID)
	assert.Equal(t, weaponUUID, *order.WeaponUUID)
}

func TestPayment_PayOrder_InvalidUUIDFormat(t *testing.T) {
	_, err := paymentClient.PayOrder(context.Background(), &paymentv1.PayOrderRequest{
		OrderUuid:     "invalid-uuid-format",
		PaymentMethod: paymentv1.PaymentMethod_PAYMENT_METHOD_CARD,
	})
	require.Error(t, err)
	testutil.AssertGRPCStatus(t, err, codes.InvalidArgument)
}

// Тесты полного жизненного цикла

func TestOrder_FullLifecycle_CreatePayGet(t *testing.T) {
	// 1. Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullTitaniumUUID,
		EngineUUID: EngineIonBUUID,
		ShieldUUID: new(ShieldEnergyUUID),
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)
	assert.NotEmpty(t, createResult.OrderUUID)

	expectedTotal := int64(HullTitaniumPrice + EngineIonBPrice + ShieldEnergyPrice)
	assert.Equal(t, expectedTotal, createResult.TotalPrice)

	// 2. Получаем заказ (проверяем PENDING_PAYMENT)
	order1, getResp1 := getOrder(t, createResult.OrderUUID)
	defer getResp1.Body.Close()
	assert.Equal(t, "PENDING_PAYMENT", order1.Status)
	assert.Nil(t, order1.TransactionUUID)

	// 3. Оплачиваем заказ
	payReq := &PayOrderRequest{PaymentMethod: "SBP"}
	payResult, payResp := payOrder(t, createResult.OrderUUID, payReq)
	defer payResp.Body.Close()
	require.NotNil(t, payResult)
	assert.NotEmpty(t, payResult.TransactionUUID)

	// 4. Получаем заказ (проверяем PAID)
	order2, getResp2 := getOrder(t, createResult.OrderUUID)
	defer getResp2.Body.Close()

	assert.Equal(t, "PAID", order2.Status)
	require.NotNil(t, order2.TransactionUUID)
	assert.Equal(t, payResult.TransactionUUID, *order2.TransactionUUID)
	require.NotNil(t, order2.PaymentMethod)
	assert.Equal(t, "SBP", *order2.PaymentMethod)
}

func TestOrder_FullLifecycle_CreateCancelGet(t *testing.T) {
	// 1. Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// 2. Получаем заказ (проверяем PENDING_PAYMENT)
	order1, getResp1 := getOrder(t, createResult.OrderUUID)
	defer getResp1.Body.Close()
	assert.Equal(t, "PENDING_PAYMENT", order1.Status)

	// 3. Отменяем заказ
	_, cancelResp := cancelOrder(t, createResult.OrderUUID)
	defer cancelResp.Body.Close()

	// 4. Получаем заказ (проверяем CANCELLED)
	order2, getResp2 := getOrder(t, createResult.OrderUUID)
	defer getResp2.Body.Close()

	assert.Equal(t, "CANCELLED", order2.Status)
	assert.Nil(t, order2.TransactionUUID)
}

func TestOrder_FullLifecycle_AllPartsPayGet(t *testing.T) {
	// Полный жизненный цикл со всеми 4 деталями: hull + engine + shield + weapon
	shieldUUID := ShieldEnergyUUID
	weaponUUID := WeaponLaserUUID
	createReq := &CreateOrderRequest{
		HullUUID:   HullTitaniumUUID,
		EngineUUID: EngineIonBUUID,
		ShieldUUID: &shieldUUID,
		WeaponUUID: &weaponUUID,
	}

	// 1. Создаём заказ
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	expectedTotal := int64(HullTitaniumPrice + EngineIonBPrice + ShieldEnergyPrice + WeaponLaserPrice)
	assert.Equal(t, expectedTotal, createResult.TotalPrice)

	// 2. Проверяем все детали в GET ответе
	order1, getResp1 := getOrder(t, createResult.OrderUUID)
	defer getResp1.Body.Close()
	assert.Equal(t, HullTitaniumUUID, order1.HullUUID)
	assert.Equal(t, EngineIonBUUID, order1.EngineUUID)
	require.NotNil(t, order1.ShieldUUID)
	assert.Equal(t, shieldUUID, *order1.ShieldUUID)
	require.NotNil(t, order1.WeaponUUID)
	assert.Equal(t, weaponUUID, *order1.WeaponUUID)

	// 3. Оплачиваем заказ
	payReq := &PayOrderRequest{PaymentMethod: "CREDIT_CARD"}
	payResult, payResp := payOrder(t, createResult.OrderUUID, payReq)
	defer payResp.Body.Close()
	require.NotNil(t, payResult)

	// 4. Проверяем финальное состояние
	order2, getResp2 := getOrder(t, createResult.OrderUUID)
	defer getResp2.Body.Close()

	assert.Equal(t, "PAID", order2.Status)
	require.NotNil(t, order2.PaymentMethod)
	assert.Equal(t, "CREDIT_CARD", *order2.PaymentMethod)
}

// Тесты отсутствия на складе (StockQuantity <= 0)

func TestOrder_Create_OutOfStock(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullOutOfStockUUID,
		EngineUUID: EngineIonCUUID,
	}

	_, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// Тесты ogen-валидации (400 Bad Request)

func TestOrder_Create_InvalidBody_EmptyJSON(t *testing.T) {
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders", bytes.NewReader([]byte("{}")))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Create_InvalidBody_NotJSON(t *testing.T) {
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders", bytes.NewReader([]byte("not json")))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Create_InvalidBody_MissingHullUUID(t *testing.T) {
	body := `{"engine_uuid": "` + EngineIonCUUID + `"}`
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders", bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Create_InvalidBody_MissingEngineUUID(t *testing.T) {
	body := `{"hull_uuid": "` + HullAluminumUUID + `"}`
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders", bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Create_InvalidBody_InvalidHullUUID(t *testing.T) {
	body := `{"hull_uuid": "not-a-uuid", "engine_uuid": "` + EngineIonCUUID + `"}`
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders", bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Get_InvalidUUIDInPath(t *testing.T) {
	resp, err := httpClient.Get(orderBaseURL() + "/api/v1/orders/not-a-uuid")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Pay_InvalidUUIDInPath(t *testing.T) {
	body := `{"payment_method": "CARD"}`
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders/not-a-uuid/pay", bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Pay_InvalidPaymentMethod(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Пытаемся оплатить невалидным методом (ogen отклонит)
	body := `{"payment_method": "BITCOIN"}`
	httpReq, err := http.NewRequest(http.MethodPost,
		orderBaseURL()+"/api/v1/orders/"+createResult.OrderUUID+"/pay",
		bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Pay_MissingPaymentMethod(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	// Пытаемся оплатить без payment_method
	body := `{}`
	httpReq, err := http.NewRequest(http.MethodPost,
		orderBaseURL()+"/api/v1/orders/"+createResult.OrderUUID+"/pay",
		bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Pay_EmptyBody(t *testing.T) {
	// Создаём заказ
	createReq := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
	}
	createResult, createResp := createOrder(t, createReq)
	defer createResp.Body.Close()
	require.NotNil(t, createResult)

	httpReq, err := http.NewRequest(http.MethodPost,
		orderBaseURL()+"/api/v1/orders/"+createResult.OrderUUID+"/pay",
		bytes.NewReader([]byte("")))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOrder_Cancel_InvalidUUIDInPath(t *testing.T) {
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders/not-a-uuid/cancel", nil)
	require.NoError(t, err)

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// Тесты с shield only (без weapon)

func TestOrder_Create_WithShieldOnly(t *testing.T) {
	req := &CreateOrderRequest{
		HullUUID:   HullAluminumUUID,
		EngineUUID: EngineIonCUUID,
		ShieldUUID: new(ShieldEnergyUUID),
	}

	result, resp := createOrder(t, req)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, result)
	expectedTotal := int64(HullAluminumPrice + EngineIonCPrice + ShieldEnergyPrice)
	assert.Equal(t, expectedTotal, result.TotalPrice)
}

// Тест inventory: деталь с нулевым остатком

func TestInventory_GetPart_OutOfStock(t *testing.T) {
	resp, err := inventoryClient.GetPart(context.Background(), &inventoryv1.GetPartRequest{
		Uuid: HullOutOfStockUUID,
	})
	require.NoError(t, err)

	part := resp.GetPart()
	assert.Equal(t, HullOutOfStockUUID, part.GetUuid())
	assert.Equal(t, int64(HullOutOfStockPrice), part.GetPrice())
	assert.Equal(t, int64(0), part.GetStockQuantity())
	assert.Equal(t, inventoryv1.PartType_PART_TYPE_HULL, part.GetPartType())
}

func TestInventory_ListParts_ByUuids_EmptyList(t *testing.T) {
	// Пустой список UUID (должен вернуть все детали при фильтрации по типу UNSPECIFIED)
	resp, err := inventoryClient.ListParts(context.Background(), &inventoryv1.ListPartsRequest{
		Uuids: []string{},
	})
	require.NoError(t, err)
	assert.Len(t, resp.GetParts(), 7)
}

func TestOrder_BadRequest_ErrorBodyFormat(t *testing.T) {
	httpReq, err := http.NewRequest(http.MethodPost, orderBaseURL()+"/api/v1/orders", bytes.NewReader([]byte("{}")))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&errResp))
	assert.Equal(t, http.StatusBadRequest, errResp.Code)
	assert.Equal(t, "некорректный запрос", errResp.Message)
}
