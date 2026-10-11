package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/ceclin/notify-qq/internal/logging"
)

// The SDK owns asynchronous exporter failures. Collector response bodies may
// contain sensitive values, and an unavailable collector must not flood stderr.
func ErrorHandler(ctx context.Context) otel.ErrorHandler {
	var nextReport atomic.Int64
	return otel.ErrorHandlerFunc(func(err error) {
		now := time.Now().UnixNano()
		next := nextReport.Load()
		if now < next || !nextReport.CompareAndSwap(next, now+int64(30*time.Second)) {
			return
		}
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelWarn, "Telemetry SDK operation failed",
			slog.String("event", "telemetry_failed"), slog.String("error_type", fmt.Sprintf("%T", err)),
		)
	})
}
