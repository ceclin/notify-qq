package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ceclin/notify-qq/internal/apikey"
	"github.com/ceclin/notify-qq/internal/config"
	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/qq"
	"github.com/ceclin/notify-qq/internal/telemetry"

	"github.com/labstack/echo/v5"
	"github.com/samber/oops"
	"go.opentelemetry.io/otel"
)

// The command owns OS signals and exit status; this boundary owns startup,
// request-serving lifetime, and one-time reporting of lifecycle failures.
func Run(ctx context.Context) (runError error) {
	ctx = logging.WithLogger(ctx, logging.NewLogger(os.Stderr, slog.LevelInfo))
	settings, err := config.Load()
	if err != nil {
		return reportStartupFailure(ctx, err)
	}
	logger := logging.NewLogger(os.Stderr, settings.LogLevel)
	ctx = logging.WithLogger(ctx, logger)
	previousErrorHandler := otel.GetErrorHandler()
	otel.SetErrorHandler(telemetry.ErrorHandler(ctx))
	defer otel.SetErrorHandler(previousErrorHandler)
	observability, err := telemetry.New(ctx, settings.Telemetry, version)
	if err != nil {
		return reportStartupFailure(ctx, err)
	}
	if observability != nil {
		logger.LogAttrs(ctx, slog.LevelInfo, "Telemetry started", slog.String("event", "telemetry_started"),
			slog.String("otel_service_name", settings.Telemetry.ServiceName), slog.Float64("trace_sample_ratio", settings.Telemetry.SampleRatio))
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			err := observability.Shutdown(shutdownCtx)
			level := slog.LevelInfo
			if err != nil {
				level = slog.LevelWarn
			}
			// Exporter failures may contain collector response bodies or credentials.
			logger.LogAttrs(shutdownCtx, level, "Telemetry stopped", slog.String("event", "telemetry_stopped"), slog.Bool("flushed", err == nil))
		}()
	}
	keys, err := openKeyStore(ctx, settings.DataDir)
	if err != nil {
		return reportStartupFailure(ctx, err)
	}
	defer func() {
		if err := keys.Close(); err != nil {
			logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "API key storage failed to close", slog.String("event", "key_store_close_failed"), logging.ErrorAttribute(err))
			runError = errors.Join(runError, err)
		}
	}()
	qqClient := qq.NewClient(qq.Config{AppID: settings.AppID, AppSecret: settings.AppSecret}, qq.WithTelemetry(observability))
	service := newService(settings.APIPathPrefix, logger, keys, qqClient, observability)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", settings.ListenAddr)
	if err != nil {
		return reportStartupFailure(ctx, oops.In("server").Wrapf(err, "HTTP listener could not start"))
	}
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "Notification service started",
		slog.String("event", "service_started"),
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("listen_addr", listener.Addr().String()),
		slog.String("api_path_prefix", settings.APIPathPrefix),
	)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	eventWorkerDone := make(chan error, 1)
	go func() {
		err := service.qq.Listen(runCtx, service.handleQQEvent)
		eventWorkerDone <- err
		if err != nil {
			service.running.Store(false)
			cancel()
		}
	}()
	server := echo.StartConfig{
		Listener: listener, HideBanner: true, HidePort: true, GracefulTimeout: 25 * time.Second,
		BeforeServeFunc: func(server *http.Server) error {
			server.ReadHeaderTimeout = 5 * time.Second
			server.ReadTimeout = 10 * time.Second
			server.WriteTimeout = 25 * time.Second
			server.IdleTimeout = 60 * time.Second
			server.MaxHeaderBytes = 32 << 10
			service.running.Store(true)
			server.RegisterOnShutdown(func() { service.running.Store(false) })
			return nil
		},
		OnShutdownError: func(err error) {
			logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "Graceful shutdown failed", slog.String("event", "shutdown_failed"), logging.ErrorAttribute(oops.In("server").Wrap(err)))
		},
	}
	serverError := server.Start(runCtx, service.routes())
	service.running.Store(false)
	cancel()
	eventError := <-eventWorkerDone
	if serverError != nil {
		err = oops.In("server").Wrap(serverError)
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "HTTP server failed", slog.String("event", "server_failed"), logging.ErrorAttribute(err))
		return err
	}
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "Notification service stopped", slog.String("event", "service_stopped"))
	return eventError
}

func reportStartupFailure(ctx context.Context, err error) error {
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "Service startup failed", slog.String("event", "startup_failed"), logging.ErrorAttribute(err))
	return err
}

func openKeyStore(ctx context.Context, directory string) (*apikey.Store, error) {
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	keys, err := apikey.Open(openCtx, directory)
	if err != nil {
		return nil, err
	}
	reportMigrations(ctx, keys)
	metadata, err := keys.List(openCtx)
	if err != nil {
		return nil, errors.Join(err, keys.Close())
	}
	active := 0
	for _, key := range metadata {
		if key.RevokedAt == nil {
			active++
		}
	}
	logger := logging.FromContext(ctx)
	logger.LogAttrs(ctx, slog.LevelInfo, "API key storage opened", slog.String("event", "api_key_store_opened"), slog.Int("active_keys", active))
	if active == 0 {
		logger.LogAttrs(ctx, slog.LevelWarn, "Create a script credential with keys create before sending notifications", slog.String("event", "api_keys_missing"))
	}
	return keys, nil
}

func reportMigrations(ctx context.Context, store *apikey.Store) {
	state := store.Migrations()
	if state.Applied > 0 {
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "Database migrations applied", slog.String("event", "database_migrated"), slog.Int64("database_version", state.Version), slog.Int("migration_count", state.Applied))
	}
}
