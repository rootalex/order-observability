package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rootalex/order-observability/clock"
	"github.com/rootalex/order-observability/domain"
)

// Unit-тесты usecase: здесь моки портов — правильный выбор. Проверяется логика
// ветвления (что делать при отказе/сбое платёжки), а не SQL и не HTTP —
// их проверяют интеграционные тесты в testing/integration.

type savedOrder struct {
	status domain.Status
	events []string
}

type fakeRepo struct {
	created, updated []savedOrder
	updateErr        error
}

func snapshot(o *domain.Order, events []domain.Event) savedOrder {
	s := savedOrder{status: o.Status}
	for _, e := range events {
		s.events = append(s.events, e.Name())
	}
	return s
}

func (r *fakeRepo) Create(_ context.Context, o *domain.Order, events []domain.Event) error {
	r.created = append(r.created, snapshot(o, events))
	return nil
}

func (r *fakeRepo) Update(_ context.Context, o *domain.Order, events []domain.Event) error {
	r.updated = append(r.updated, snapshot(o, events))
	return r.updateErr
}

func (r *fakeRepo) Get(context.Context, domain.OrderID) (*domain.Order, error) {
	return nil, ErrOrderNotFound
}

type fakePayment struct {
	got ChargeRequest
	res ChargeResult
	err error
}

func (p *fakePayment) Charge(_ context.Context, req ChargeRequest) (ChargeResult, error) {
	p.got = req
	return p.res, p.err
}

type fixedID domain.OrderID

func (f fixedID) NewOrderID() domain.OrderID { return domain.OrderID(f) }

var fixedNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func validRequest() *CreateOrderRequest {
	return &CreateOrderRequest{
		CustomerID: "c-1",
		Tier:       domain.TierPremium,
		Items:      []domain.Item{{ProductID: "p-1", Quantity: 3, PriceCents: 500}},
	}
}

func TestCreateOrder_Success(t *testing.T) {
	repo, pay := &fakeRepo{}, &fakePayment{res: ChargeResult{PaymentID: "pay-1"}}
	uc := NewCreateOrder(repo, pay, fixedID("ord-1"), clock.NewFake(fixedNow), nil)

	resp, err := uc.Execute(context.Background(), validRequest())

	require.NoError(t, err)
	assert.Equal(t, domain.StatusPaid, resp.Status)
	assert.Equal(t, "pay-1", resp.PaymentID)
	assert.Equal(t, ChargeRequest{OrderID: "ord-1", AmountCents: 1500, Currency: "USD", IdempotencyKey: "ord-1"}, pay.got)

	assert.Equal(t, []savedOrder{{status: domain.StatusPending, events: []string{"order.created"}}}, repo.created)
	assert.Equal(t, []savedOrder{{status: domain.StatusPaid, events: []string{"order.paid"}}}, repo.updated)

	require.Len(t, resp.Events, 2)
	// Время детерминировано: FakeClock, никакого time.Now() в тесте.
	assert.Equal(t, fixedNow, resp.Events[0].OccurredAt())
}

func TestCreateOrder_PaymentOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		payErr     error
		wantReason string
		wantIs     error
	}{
		{name: "declined", payErr: ErrPaymentDeclined, wantReason: "payment_declined", wantIs: ErrPaymentDeclined},
		{name: "gateway error", payErr: errors.New("timeout"), wantReason: "payment_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{}
			uc := NewCreateOrder(repo, &fakePayment{err: tt.payErr}, fixedID("ord-1"), clock.NewFake(fixedNow), nil)

			resp, err := uc.Execute(context.Background(), validRequest())

			require.Error(t, err)
			if tt.wantIs != nil {
				assert.ErrorIs(t, err, tt.wantIs)
			}
			require.NotNil(t, resp, "caller needs the order id and status even on payment failure")
			assert.Equal(t, domain.StatusFailed, resp.Status)
			assert.Equal(t, []savedOrder{{status: domain.StatusFailed, events: []string{"order.failed"}}}, repo.updated)

			failed, ok := resp.Events[len(resp.Events)-1].(domain.OrderFailed)
			require.True(t, ok)
			assert.Equal(t, tt.wantReason, failed.Reason)
		})
	}
}

func TestCreateOrder_InvalidRequestDoesNotTouchPayment(t *testing.T) {
	repo, pay := &fakeRepo{}, &fakePayment{}
	uc := NewCreateOrder(repo, pay, fixedID("ord-1"), clock.NewFake(fixedNow), nil)

	_, err := uc.Execute(context.Background(), &CreateOrderRequest{CustomerID: "c-1"})

	assert.ErrorIs(t, err, domain.ErrEmptyOrder)
	assert.Empty(t, repo.created)
	assert.Zero(t, pay.got)
}

// Сценарий из Q3: деньги списаны, а сохранить статус не удалось.
// Usecase обязан вернуть ошибку — это сигнал для алерта и reconciliation.
func TestCreateOrder_ChargedButUpdateFailed(t *testing.T) {
	repo := &fakeRepo{updateErr: errors.New("db down")}
	uc := NewCreateOrder(repo, &fakePayment{res: ChargeResult{PaymentID: "pay-1"}}, fixedID("ord-1"), clock.NewFake(fixedNow), nil)

	_, err := uc.Execute(context.Background(), validRequest())

	assert.ErrorContains(t, err, "update order")
}
