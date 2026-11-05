package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/samber/oops"

	"github.com/ceclin/notify-qq/internal/config"
	"github.com/ceclin/notify-qq/internal/logging"
)

func healthcheck(ctx context.Context) error {
	address, err := config.HealthcheckURL()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return logging.OperationError(ctx, "healthcheck", "healthcheck_failed", "Readiness request could not be created", err)
	}
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return logging.OperationError(ctx, "healthcheck", "healthcheck_failed", "Readiness request failed", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return logging.WithErrorAttributes(oops.FromContext(ctx).In("healthcheck").Code("healthcheck_failed"), slog.Int("http_status", response.StatusCode)).New("Service is not ready")
	}
	return nil
}
