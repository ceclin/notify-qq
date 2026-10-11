package logging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/samber/oops"
	"go.opentelemetry.io/otel/trace"
)

type requestLoggerKey struct{}

func NewLogger(output io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
	return slog.New(traceHandler{handler.WithAttrs([]slog.Attr{slog.String("service", "notify-qq")})})
}

type traceHandler struct{ slog.Handler }

func (handler traceHandler) Handle(ctx context.Context, record slog.Record) error {
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		record.AddAttrs(slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()))
	}
	return handler.Handler.Handle(ctx, record)
}

func (handler traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{handler.Handler.WithAttrs(attrs)}
}

func (handler traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{handler.Handler.WithGroup(name)}
}

// Request fields follow the context into QQ calls and recipient persistence;
// operations outside an HTTP request use the application logger.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(requestLoggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

// Echo wraps Oops errors in HTTPError. Unwrap at the logging boundary so slog
// resolves Oops' native LogValuer and retains its context and stacktrace.
func ErrorAttribute(err error) slog.Attr {
	if detailed, ok := errors.AsType[oops.OopsError](err); ok {
		return slog.Any("error", detailed)
	}
	return slog.Any("error", err)
}

// Oops.With still accepts key/value pairs. Keep callers typed and convert at
// this boundary so attributes aren't silently ignored by its key/value API.
func WithErrorAttributes(builder oops.OopsErrorBuilder, attrs ...slog.Attr) oops.OopsErrorBuilder {
	for _, attr := range attrs {
		builder = builder.With(attr.Key, attr.Value.Any())
	}
	return builder
}

func Milliseconds(duration time.Duration) float64 { return duration.Seconds() * 1000 }

func OperationError(ctx context.Context, domain, code, message string, cause error) error {
	builder := oops.FromContext(ctx).In(domain).Code(code)
	if cause == nil {
		return builder.New(message)
	}
	return builder.Wrapf(cause, "%s", message)
}

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, requestLoggerKey{}, logger)
}
