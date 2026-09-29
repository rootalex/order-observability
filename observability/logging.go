package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/usecase"
)

// Log types: operational is system health (requests, durations, failures),
// business is facts of the domain (domain events).
const (
	logTypeKey  = "log_type"
	operational = "operational"
	business    = "business"
)

// NewLogger returns a JSON logger with trace_id/span_id in every record and PII redaction.
// Log through the *Context methods (InfoContext, ErrorContext), otherwise trace_id is missing.
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: RedactPII})
	return slog.New(NewTraceHandler(h))
}

// TraceHandler adds trace_id and span_id from ctx to every record.
type TraceHandler struct {
	slog.Handler
}

func NewTraceHandler(h slog.Handler) *TraceHandler { return &TraceHandler{Handler: h} }

func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithGroup(name)}
}

// Second line of defense against PII leaks. The first one is logging allowlisted fields only.
var sensitiveKeys = map[string]struct{}{
	"email": {}, "phone": {}, "card": {}, "card_number": {}, "cvv": {},
	"password": {}, "token": {}, "authorization": {}, "address": {},
}

const redacted = "[REDACTED]"

// RedactPII is a slog ReplaceAttr that masks values of sensitive keys at any nesting level.
func RedactPII(_ []string, a slog.Attr) slog.Attr {
	if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, redacted)
	}
	return a
}

// isExpected separates expected business outcomes from failures: the former must not
// produce ERROR logs, span status Error or alerts.
func isExpected(err error) bool {
	return errors.Is(err, domain.ErrEmptyOrder) ||
		errors.Is(err, domain.ErrInvalidQuantity) ||
		errors.Is(err, usecase.ErrPaymentDeclined) ||
		errors.Is(err, usecase.ErrPaymentPending)
}

// stackError keeps the stack of the point where the error originated (usually an adapter).
type stackError struct {
	err   error
	stack []uintptr
}

func (e *stackError) Error() string { return e.err.Error() }
func (e *stackError) Unwrap() error { return e.err }

// WithStack attaches the call stack to err. An existing stack is kept.
func WithStack(err error) error {
	if err == nil {
		return nil
	}
	var se *stackError
	if errors.As(err, &se) {
		return err
	}
	return &stackError{err: err, stack: callers(3)}
}

// ErrorAttr returns an "error" group with message and stack. If err carries no stack,
// the stack of the logging point is used: it at least shows where the error crossed the boundary.
func ErrorAttr(err error) slog.Attr {
	var se *stackError
	pcs := callers(3)
	if errors.As(err, &se) {
		pcs = se.stack
	}
	return slog.Group("error",
		slog.String("message", err.Error()),
		slog.String("stack", formatStack(pcs)),
	)
}

func callers(skip int) []uintptr {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(skip, pcs)
	return pcs[:n]
}

func formatStack(pcs []uintptr) string {
	var b strings.Builder
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		fmt.Fprintf(&b, "%s\n\t%s:%d\n", f.Function, f.File, f.Line)
		if !more {
			break
		}
	}
	return b.String()
}

// LoggingInteractor logs request and response at the usecase boundary (option C)
// and domain events as business logs.
type LoggingInteractor struct {
	inner usecase.CreateOrderInteractor
	log   *slog.Logger
}

func NewLoggingInteractor(inner usecase.CreateOrderInteractor, log *slog.Logger) *LoggingInteractor {
	return &LoggingInteractor{inner: inner, log: log}
}

func (l *LoggingInteractor) Execute(ctx context.Context, req *usecase.CreateOrderRequest) (*usecase.CreateOrderResponse, error) {
	start := time.Now()
	// Allowlisted fields only: req is never logged as a whole (it contains Email).
	l.log.InfoContext(ctx, "create order started",
		slog.String(logTypeKey, operational),
		slog.Group("request",
			slog.String("customer_id", string(req.CustomerID)),
			slog.String("customer_tier", string(req.Tier)),
			slog.Int("items_count", len(req.Items)),
		),
	)

	resp, err := l.inner.Execute(ctx, req)

	attrs := []any{
		slog.String(logTypeKey, operational),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if resp != nil {
		attrs = append(attrs, slog.Group("response",
			slog.String("order_id", string(resp.OrderID)),
			slog.String("status", string(resp.Status)),
		))
		l.logDomainEvents(ctx, resp)
	}
	switch {
	case errors.Is(err, usecase.ErrPaymentPending):
		// Not a rejection: the order exists and waits for the charge outcome to be reconciled.
		l.log.WarnContext(ctx, "create order pending payment confirmation", append(attrs, slog.String("reason", err.Error()))...)
		return resp, err
	case err != nil && isExpected(err):
		// An expected business outcome (invalid order, declined payment) is not a failure: WARN, no stack.
		l.log.WarnContext(ctx, "create order rejected", append(attrs, slog.String("reason", err.Error()))...)
		return resp, err
	case err != nil:
		l.log.ErrorContext(ctx, "create order failed", append(attrs, ErrorAttr(err))...)
		return resp, err
	}
	l.log.InfoContext(ctx, "create order completed", attrs...)
	return resp, nil
}

func (l *LoggingInteractor) logDomainEvents(ctx context.Context, resp *usecase.CreateOrderResponse) {
	for _, e := range resp.Events {
		attrs := []any{
			slog.String(logTypeKey, business),
			slog.String("order_id", string(resp.OrderID)),
			slog.Time("occurred_at", e.OccurredAt()),
		}
		if f, ok := e.(domain.OrderFailed); ok {
			attrs = append(attrs, slog.String("reason", f.Reason))
		}
		if resp.PaymentID != "" {
			attrs = append(attrs, slog.String("payment_id", resp.PaymentID))
		}
		l.log.InfoContext(ctx, e.Name(), attrs...)
	}
}
