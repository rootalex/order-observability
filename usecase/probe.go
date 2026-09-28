package usecase

import "context"

// Step — шаг обработки заказа. Набор значений ограничен: он же лейбл метрики.
type Step string

const (
	StepValidation  Step = "validation"
	StepPayment     Step = "payment"
	StepFulfillment Step = "fulfillment"
)

// Probe — порт наблюдаемости шагов usecase (Domain-Oriented Observability).
// Usecase сообщает «что происходит», а реализация в observability решает,
// как это превратить в span'ы и метрики. Usecase не импортирует OTel/Prometheus.
type Probe interface {
	// StepStarted начинает шаг; возвращённый ctx нужно передать в вызовы внутри шага,
	// а done — вызвать по завершении с ошибкой шага (или nil).
	StepStarted(ctx context.Context, step Step) (context.Context, func(err error))
}

type noopProbe struct{}

func (noopProbe) StepStarted(ctx context.Context, _ Step) (context.Context, func(error)) {
	return ctx, func(error) {}
}
