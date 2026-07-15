package repository

import (
	"fmt"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type OrderRepository struct {
	DB *gorm.DB
}

type OrderListFilter struct {
	Status     *model.OrderStatus
	CoinSymbol string
	Limit      int
}

type OrderExecutionBatchUpdate struct {
	OrderID           uint
	FilledAmount      decimal.Decimal
	FilledQuoteAmount decimal.Decimal
	Status            model.OrderStatus
}

func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{DB: db}
}

func (r *OrderRepository) WithTx(tx *gorm.DB) *OrderRepository {
	return &OrderRepository{DB: tx}
}

func (r *OrderRepository) CreateOrder(order *model.Order) error {
	return r.DB.Create(order).Error
}

func (r *OrderRepository) UpdateOrderStatus(orderID uint, status model.OrderStatus, filledAmount decimal.Decimal) error {
	return r.UpdateOrderFill(orderID, filledAmount, status)
}

func (r *OrderRepository) UpdateOrderFill(orderID uint, filledAmount decimal.Decimal, status model.OrderStatus) error {
	return r.UpdateOrderExecution(orderID, filledAmount, decimal.Zero, status)
}

func (r *OrderRepository) UpdateOrderExecution(orderID uint, filledAmount decimal.Decimal, filledQuoteAmount decimal.Decimal, status model.OrderStatus) error {
	result := r.DB.Model(&model.Order{}).Where("id = ?", orderID).Updates(map[string]interface{}{
		"status":              status,
		"filled_amount":       filledAmount,
		"filled_quote_amount": filledQuoteAmount,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("order fill update affected no rows")
	}
	return nil
}

func (r *OrderRepository) FindByID(orderID uint) (*model.Order, error) {
	var order model.Order
	err := r.DB.First(&order, orderID).Error
	return &order, err
}

func (r *OrderRepository) FindByUserIDAndID(userID uint, orderID uint) (*model.Order, error) {
	var order model.Order
	err := r.DB.Where("user_id = ? AND id = ?", userID, orderID).First(&order).Error
	return &order, err
}

func (r *OrderRepository) ListByUserID(userID uint, filter OrderListFilter) ([]model.Order, error) {
	var orders []model.Order
	query := r.DB.Where("user_id = ?", userID)
	if filter.Status != nil {
		query = query.Where("status = ?", *filter.Status)
	}
	if filter.CoinSymbol != "" {
		query = query.Where("coin_symbol = ?", filter.CoinSymbol)
	}
	err := query.
		Order("created_at DESC").
		Order("id DESC").
		Limit(filter.Limit).
		Find(&orders).Error
	return orders, err
}

func (r *OrderRepository) FindByIDForUpdate(orderID uint) (*model.Order, error) {
	var order model.Order
	err := r.DB.Clauses(clause.Locking{Strength: "UPDATE"}).First(&order, orderID).Error
	return &order, err
}

// FindOpenMarketOrders는 부팅 시 파이널라이저용으로 PENDING/PARTIAL 시장가 주문을
// 조회합니다. 시장가는 오더북에 rest하지 않으므로 리플레이가 끝난 부팅 시점에
// 이 상태로 남은 주문은 더 이상 체결될 수 없습니다.
func (r *OrderRepository) FindOpenMarketOrders() ([]model.Order, error) {
	var orders []model.Order
	err := r.DB.
		Where("status IN ?", []model.OrderStatus{model.OrderStatusPending, model.OrderStatusPartial}).
		Where("order_type = ?", model.OrderTypeMarket).
		Order("id ASC").
		Find(&orders).Error
	return orders, err
}

func (r *OrderRepository) FindOpenOrdersForBootstrap() ([]model.Order, error) {
	var orders []model.Order
	err := r.DB.
		Where("status IN ?", []model.OrderStatus{model.OrderStatusPending, model.OrderStatusPartial}).
		Where("order_type = ?", model.OrderTypeLimit).
		Where("amount > filled_amount").
		Order("created_at ASC").
		Order("id ASC").
		Find(&orders).Error
	return orders, err
}

// LockByIDs는 배치 정산용으로 주문들을 ID 오름차순 FOR UPDATE로 잠급니다.
// 요청한 ID 수와 잠근 행 수가 다르면 에러입니다 — 배치는 부분 성공이 없습니다.
func (r *OrderRepository) LockByIDs(ids []uint) ([]model.Order, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var orders []model.Order
	err := r.DB.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).
		Order("id ASC").
		Find(&orders).Error
	if err != nil {
		return nil, err
	}
	if len(orders) != len(ids) {
		return nil, fmt.Errorf("order lock expected %d rows, locked %d", len(ids), len(orders))
	}
	return orders, nil
}

func (r *OrderRepository) BatchUpdateExecutions(updates []OrderExecutionBatchUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	rows := make([]string, 0, len(updates))
	args := make([]interface{}, 0, len(updates)*4)
	for i, u := range updates {
		base := i * 4
		rows = append(rows, fmt.Sprintf(
			"($%d::bigint, $%d::text, $%d::numeric, $%d::numeric)",
			base+1, base+2, base+3, base+4,
		))
		args = append(args, u.OrderID, string(u.Status), u.FilledAmount, u.FilledQuoteAmount)
	}
	sql := fmt.Sprintf(`
		UPDATE orders AS o
		SET
			status = v.status,
			filled_amount = v.filled_amount,
			filled_quote_amount = v.filled_quote_amount
		FROM (VALUES %s) AS v(id, status, filled_amount, filled_quote_amount)
		WHERE o.id = v.id`,
		strings.Join(rows, ", "),
	)
	result := r.DB.Exec(sql, args...)
	if result.Error != nil {
		return result.Error
	}
	if int(result.RowsAffected) != len(updates) {
		return fmt.Errorf("order batch update affected %d rows, expected %d", result.RowsAffected, len(updates))
	}
	return nil
}
