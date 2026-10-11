package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/ceclin/notify-qq/internal/logging"
)

const readinessTimeout = time.Second
const readinessLogHandled = "notify_qq_readiness_log_handled"

func (s *service) readiness(c *echo.Context) error {
	ctx := c.Request().Context()
	// Readiness failures are reported on transitions here, keeping repeated probe
	// failures out of the generic HTTP error log while preserving their responses.
	c.Set(readinessLogHandled, true)
	if !s.running.Load() {
		return httpFailure(ctx, http.StatusServiceUnavailable, "service_not_ready", "Service is not ready", nil)
	}
	probeCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	if err := s.keys.CheckReadiness(probeCtx); err != nil {
		if !s.readinessFailed.Swap(true) {
			logging.FromContext(ctx).LogAttrs(ctx, slog.LevelWarn, "Service readiness degraded", slog.String("event", "readiness_changed"), slog.Bool("ready", false), slog.String("reason", "storage_unavailable"), logging.ErrorAttribute(err))
		}
		return httpFailure(ctx, http.StatusServiceUnavailable, "service_not_ready", "Service is not ready", err)
	}
	// Shutdown may have started during the bounded database probe.
	if !s.running.Load() {
		return httpFailure(ctx, http.StatusServiceUnavailable, "service_not_ready", "Service is not ready", nil)
	}
	if s.readinessFailed.Swap(false) {
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "Service readiness restored", slog.String("event", "readiness_changed"), slog.Bool("ready", true))
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
}
