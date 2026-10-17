package service

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	inventoryv1 "github.com/aidar/shared/pkg/proto/inventory/v1"
)

// Part представляет деталь космического корабля
type Part struct {
	UUID          string
	Name          string
	Description   string
	Price         int64 // в копейках
	PartType      inventoryv1.PartType
	StockQuantity int64
	CreatedAt     *timestamppb.Timestamp
}

// server реализует gRPC сервис
type server struct {
	inventoryv1.UnimplementedInventoryServiceServer
	parts map[uuid.UUID]Part
}

// NewServer создаёт сервер с предзагруженными seed-данными
func NewServer() *server {
	now := timestamppb.Now()

	return &server{
		parts: map[uuid.UUID]Part{
			uuid.MustParse("550e8400-e29b-41d4-a716-446655440001"): {
				UUID:          "550e8400-e29b-41d4-a716-446655440001",
				Name:          "Алюминиевый корпус",
				Description:   "Лёгкий корпус для небольших кораблей",
				Price:         500000, // 5000₽
				PartType:      inventoryv1.PartType_PART_TYPE_HULL,
				StockQuantity: 10,
				CreatedAt:     now,
			},
			uuid.MustParse("550e8400-e29b-41d4-a716-446655440002"): {
				UUID:          "550e8400-e29b-41d4-a716-446655440002",
				Name:          "Титановый корпус",
				Description:   "Прочный корпус для средних кораблей",
				Price:         1500000, // 15000₽
				PartType:      inventoryv1.PartType_PART_TYPE_HULL,
				StockQuantity: 5,
				CreatedAt:     now,
			},
			uuid.MustParse("550e8400-e29b-41d4-a716-446655440003"): {
				UUID:          "550e8400-e29b-41d4-a716-446655440003",
				Name:          "Ионный двигатель C",
				Description:   "Базовый ионный двигатель класса C",
				Price:         300000, // 3000₽
				PartType:      inventoryv1.PartType_PART_TYPE_ENGINE,
				StockQuantity: 8,
				CreatedAt:     now,
			},
			uuid.MustParse("550e8400-e29b-41d4-a716-446655440004"): {
				UUID:          "550e8400-e29b-41d4-a716-446655440004",
				Name:          "Ионный двигатель B",
				Description:   "Улучшенный ионный двигатель класса B",
				Price:         800000, // 8000₽
				PartType:      inventoryv1.PartType_PART_TYPE_ENGINE,
				StockQuantity: 3,
				CreatedAt:     now,
			},
			uuid.MustParse("550e8400-e29b-41d4-a716-446655440005"): {
				UUID:          "550e8400-e29b-41d4-a716-446655440005",
				Name:          "Энергетический щит",
				Description:   "Стандартный энергетический щит",
				Price:         400000, // 4000₽
				PartType:      inventoryv1.PartType_PART_TYPE_SHIELD,
				StockQuantity: 6,
				CreatedAt:     now,
			},
			uuid.MustParse("550e8400-e29b-41d4-a716-446655440006"): {
				UUID:          "550e8400-e29b-41d4-a716-446655440006",
				Name:          "Лазерная пушка",
				Description:   "Точная лазерная пушка",
				Price:         250000, // 2500₽
				PartType:      inventoryv1.PartType_PART_TYPE_WEAPON,
				StockQuantity: 7,
				CreatedAt:     now,
			},
			uuid.MustParse("550e8400-e29b-41d4-a716-446655440007"): {
				UUID:          "550e8400-e29b-41d4-a716-446655440007",
				Name:          "Плазменный корпус",
				Description:   "Экспериментальный корпус (нет на складе)",
				Price:         2000000, // 20000₽
				PartType:      inventoryv1.PartType_PART_TYPE_HULL,
				StockQuantity: 0,
				CreatedAt:     now,
			},
		},
	}
}

// toProto преобразует Part в inventoryv1.Part
func (p Part) toProto() *inventoryv1.Part {
	return &inventoryv1.Part{
		Uuid:          p.UUID,
		Name:          p.Name,
		Description:   p.Description,
		Price:         p.Price,
		PartType:      p.PartType,
		StockQuantity: p.StockQuantity,
		CreatedAt:     p.CreatedAt,
	}
}

// GetPart возвращает деталь по UUID
func (s *server) GetPart(
	ctx context.Context,
	req *inventoryv1.GetPartRequest,
) (*inventoryv1.GetPartResponse, error) {
	if req.GetUuid() == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid не указан")
	}

	id, err := uuid.Parse(req.GetUuid())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "неверный формат uuid: %s", req.GetUuid())
	}

	part, ok := s.parts[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "деталь с uuid %s не найдена", req.GetUuid())
	}

	return &inventoryv1.GetPartResponse{Part: part.toProto()}, nil
}

// ListParts возвращает список деталей с опциональной фильтрацией по типу
func (s *server) ListParts(
	ctx context.Context,
	req *inventoryv1.ListPartsRequest,
) (*inventoryv1.ListPartsResponse, error) {
	if len(req.GetUuids()) > 0 {
		parts := make([]*inventoryv1.Part, 0, len(req.GetUuids()))
		for _, rawUUID := range req.GetUuids() {
			id, err := uuid.Parse(rawUUID)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "неверный формат uuid: %s", rawUUID)
			}

			part, ok := s.parts[id]
			if !ok {
				return nil, status.Errorf(codes.NotFound, "деталь с uuid %s не найдена", rawUUID)
			}

			parts = append(parts, part.toProto())
		}

		return &inventoryv1.ListPartsResponse{Parts: parts}, nil
	}

	var parts []*inventoryv1.Part
	for _, part := range s.parts {
		if req.GetPartType() == inventoryv1.PartType_PART_TYPE_UNSPECIFIED || part.PartType == req.GetPartType() {
			parts = append(parts, part.toProto())
		}
	}

	sort.Slice(parts, func(i, j int) bool {
		return parts[i].GetName() < parts[j].GetName()
	})

	return &inventoryv1.ListPartsResponse{Parts: parts}, nil
}
