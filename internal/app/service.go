package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/oklog/ulid/v2"
	"github.com/samber/oops"

	"github.com/ceclin/notify-qq/internal/apikey"
	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/qq"
	"github.com/ceclin/notify-qq/internal/recipient"
	"github.com/ceclin/notify-qq/internal/telemetry"
)

const maxRequestBytes = 64 << 10
const requestTimeout = 20 * time.Second

type service struct {
	apiPathPrefix   string
	logger          *slog.Logger
	qq              *qq.Client
	keys            *apikey.Store
	telemetry       *telemetry.Runtime
	running         atomic.Bool
	readinessFailed atomic.Bool
}

// Dependency owners close resources after requests and event workers finish.
// Assembly itself performs no I/O and does not change the process logger.
func newService(apiPathPrefix string, logger *slog.Logger, keys *apikey.Store, qqClient *qq.Client, observability *telemetry.Runtime) *service {
	return &service{apiPathPrefix: apiPathPrefix, logger: logger, keys: keys, qq: qqClient, telemetry: observability}
}

func (s *service) routes() *echo.Echo {
	app := echo.NewWithConfig(echo.Config{Logger: s.logger, HTTPErrorHandler: handleHTTPError})
	app.Use(s.requestBoundary)
	api := app.Group(s.apiPathPrefix)
	api.GET("/healthz", func(c *echo.Context) error { return c.JSON(http.StatusOK, map[string]string{"status": "ok"}) })
	api.GET("/readyz", s.readiness)
	api.GET("/status", s.status, s.authenticate)
	api.POST("/notify", s.notify, s.authenticate)
	return app
}

func (s *service) requestBoundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		started := time.Now()
		requestID := ulid.Make().String()
		c.Response().Header().Set("X-Request-ID", requestID)
		c.Response().Header().Set("X-Content-Type-Options", "nosniff")
		c.Response().Header().Set("Cache-Control", "no-store")
		logger := slog.New(c.Logger().Handler().WithAttrs([]slog.Attr{
			slog.String("request_id", requestID),
			slog.String("method", c.Request().Method),
			slog.String("route", c.Path()),
		}))
		c.SetLogger(logger)
		ctx := c.Request().Context()
		complete := func(int) {}
		if c.Path() != s.apiPathPrefix+"/healthz" && c.Path() != s.apiPathPrefix+"/readyz" {
			ctx, complete = s.telemetry.StartRequest(ctx, c.Request().Header, c.Request().Method, c.Path())
		}
		defer func() {
			_, status := echo.ResolveResponseStatus(c.Response(), nil)
			complete(status)
		}()
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		ctx = logging.WithLogger(ctx, logger)
		builder := logging.WithErrorAttributes(oops.In("http").Trace(requestID), slog.String("request_id", requestID), slog.String("route", c.Path()))
		ctx = oops.WithBuilder(ctx, builder)
		c.SetRequest(c.Request().WithContext(ctx))
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxRequestBytes)
		var handlerError error
		panicError := builder.Code("panic_recovered").Recover(func() { handlerError = next(c) })
		if panicError != nil {
			handlerError = httpFailure(ctx, 500, "internal_error", "Internal server error", panicError)
		}
		if handlerError != nil {
			c.Echo().HTTPErrorHandler(c, handlerError)
		}
		_, status := echo.ResolveResponseStatus(c.Response(), nil)
		level := slog.LevelInfo
		if c.Path() == s.apiPathPrefix+"/healthz" || c.Path() == s.apiPathPrefix+"/readyz" || c.Path() == s.apiPathPrefix+"/status" || status >= 400 {
			level = slog.LevelDebug
		}
		c.Logger().LogAttrs(c.Request().Context(), level, "HTTP request completed", slog.String("event", "http_request_completed"), slog.Int("http_status", status), slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))))
		return nil
	}
}

func httpFailure(ctx context.Context, status int, code, public string, cause error) error {
	builder := oops.FromContext(ctx).In("http").Code(code).Public(public)
	var err error
	if cause == nil {
		err = builder.New("HTTP request rejected")
	} else {
		err = builder.Wrapf(cause, "HTTP operation failed")
	}
	return echo.NewHTTPError(status, public).Wrap(err)
}

func handleHTTPError(c *echo.Context, err error) {
	status := echo.StatusCode(err)
	if status == 0 {
		status = http.StatusInternalServerError
	}
	public := http.StatusText(status)
	if httpError, ok := errors.AsType[*echo.HTTPError](err); ok {
		status = httpError.Code
		public = httpError.Message
	}
	code := "internal_error"
	if status < 500 {
		code = "request_rejected"
	}
	if detailed, ok := errors.AsType[oops.OopsError](err); ok {
		if value, ok := detailed.Code().(string); ok {
			code = value
		}
	}
	ctx := c.Request().Context()
	level := slog.LevelDebug
	if status == 429 || status == 504 || errors.Is(err, context.Canceled) {
		level = slog.LevelWarn
	} else if status >= 500 {
		level = slog.LevelError
	}
	if handled, _ := c.Get(readinessLogHandled).(bool); !handled {
		logging.FromContext(ctx).LogAttrs(ctx, level, "HTTP request failed", slog.String("event", "http_request_failed"), slog.Int("http_status", status), slog.String("result_code", code), logging.ErrorAttribute(err))
	}
	response, _ := echo.UnwrapResponse(c.Response())
	if response != nil && response.Committed {
		return
	}
	payload := map[string]any{"error": map[string]any{"code": code, "message": public}, "request_id": c.Response().Header().Get("X-Request-ID")}
	if upstream, ok := errors.AsType[*qq.Error](err); ok {
		payload["qq"] = upstream
	}
	// A client that has disconnected cannot receive an error response; the owning
	// boundary has already recorded the failure, so don't emit a second error.
	_ = c.JSON(status, payload)
}

type authenticatedKeyContextKey struct{}

func authenticatedKey(c *echo.Context) apikey.Key {
	return c.Request().Context().Value(authenticatedKeyContextKey{}).(apikey.Key)
}

func recipientReference(target recipient.Target) string {
	digest := sha256.Sum256([]byte(string(target.Type) + ":" + target.OpenID))
	return hex.EncodeToString(digest[:8])
}

func (s *service) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		scheme, secret, found := strings.Cut(c.Request().Header.Get("Authorization"), " ")
		unauthorized := func() error {
			c.Response().Header().Set("WWW-Authenticate", "Bearer")
			return httpFailure(ctx, 401, "unauthorized", "A valid Bearer API key is required", nil)
		}
		if !found || !strings.EqualFold(scheme, "Bearer") || len(secret) < 32 || strings.ContainsAny(secret, " \t\r\n") {
			return unauthorized()
		}
		key, valid, err := s.keys.Authenticate(ctx, secret)
		if err != nil {
			return httpFailure(ctx, 500, "authentication_failed", "Authentication could not be checked", err)
		}
		if !valid {
			return unauthorized()
		}
		logger := slog.New(c.Logger().Handler().WithAttrs([]slog.Attr{slog.String("api_key_id", key.ID), slog.String("api_key_name", key.Name)}))
		c.SetLogger(logger)
		ctx = logging.WithLogger(ctx, logger)
		ctx = context.WithValue(ctx, authenticatedKeyContextKey{}, key)
		ctx = oops.WithBuilder(ctx, logging.WithErrorAttributes(oops.FromContext(ctx), slog.String("api_key_id", key.ID)))
		c.SetRequest(c.Request().WithContext(ctx))
		logger.LogAttrs(ctx, slog.LevelDebug, "API key authenticated", slog.String("event", "api_key_authenticated"))
		return next(c)
	}
}

func (s *service) status(c *echo.Context) error {
	target := authenticatedKey(c).Recipient
	return c.JSON(http.StatusOK, map[string]any{"paired": target != nil, "recipient": target, "websocket_connected": s.qq.GatewayConnected()})
}

func (s *service) notify(c *echo.Context) error {
	ctx := c.Request().Context()
	mediaType, _, err := mime.ParseMediaType(c.Request().Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return httpFailure(ctx, 415, "unsupported_media_type", "Content-Type must be application/json", nil)
	}
	var input notification
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return invalidBody(ctx, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return httpFailure(ctx, 400, "invalid_json", "Request must contain one JSON object", nil)
		}
		return invalidBody(ctx, err)
	}
	message, err := input.compile(ctx)
	if err != nil {
		return httpFailure(ctx, 400, "invalid_notification", oops.GetPublic(err, "Notification input is invalid"), err)
	}
	target := authenticatedKey(c).Recipient
	if target == nil {
		return httpFailure(ctx, 409, "recipient_not_paired", "Bind this API key by sending its pairing code to the bot", nil)
	}
	logger := logging.FromContext(ctx)
	logger.LogAttrs(ctx, slog.LevelInfo, "Notification send started",
		slog.String("event", "notification_started"),
		slog.String("recipient_type", string(target.Type)),
		slog.String("recipient_ref", recipientReference(*target)),
		slog.Int("content_bytes", len(message.Markdown)),
		slog.String("format", "markdown"),
		slog.Int("button_count", len(message.Buttons)),
	)
	result, err := s.qq.Send(ctx, *target, message)
	if err != nil {
		status := http.StatusBadGateway
		public := "QQ could not accept the notification; check the QQ error code and bot permissions"
		var upstream *qq.Error
		if errors.As(err, &upstream) && upstream.RateLimited() {
			status = http.StatusTooManyRequests
			public = "QQ rate limit reached; wait before trying again"
			retryAfter := upstream.RetryAfter
			if retryAfter == "" {
				retryAfter = "60"
			}
			c.Response().Header().Set("Retry-After", retryAfter)
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			public = "QQ request timed out; delivery is uncertain"
		}
		return httpFailure(ctx, status, "notification_failed", public, err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "Notification accepted by QQ",
		slog.String("event", "notification_accepted"),
		slog.String("recipient_type", string(target.Type)),
		slog.String("recipient_ref", recipientReference(*target)),
		slog.String("message_id", result.ID),
	)
	return c.JSON(http.StatusOK, map[string]any{"status": "accepted", "message_id": result.ID, "timestamp": result.Timestamp})
}

func invalidBody(ctx context.Context, err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return httpFailure(ctx, 413, "request_too_large", "Request exceeds 64 KiB", nil)
	}
	return httpFailure(ctx, 400, "invalid_json", "Request must contain a valid JSON object with supported fields", nil)
}

func (s *service) handleQQEvent(ctx context.Context, event qq.Event) error {
	if event.Type != "C2C_MESSAGE_CREATE" && event.Type != "GROUP_AT_MESSAGE_CREATE" {
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "QQ event requires no action", slog.String("event", "qq_event_ignored"))
		return nil
	}
	return s.replyFromMessage(ctx, event.Type, event.Data)
}

func (s *service) replyFromMessage(ctx context.Context, eventType string, data json.RawMessage) error {
	var message struct {
		ID          string `json:"id"`
		Content     string `json:"content"`
		GroupOpenID string `json:"group_openid"`
		Author      struct {
			UserOpenID   string `json:"user_openid"`
			MemberOpenID string `json:"member_openid"`
		} `json:"author"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return logging.OperationError(ctx, "qq_reply", "invalid_message", "QQ message event is invalid", err)
	}
	target := recipient.Target{Type: recipient.User, OpenID: message.Author.UserOpenID}
	sender := message.Author.UserOpenID
	if eventType == "GROUP_AT_MESSAGE_CREATE" {
		target = recipient.Target{Type: recipient.Group, OpenID: message.GroupOpenID}
		sender = message.Author.MemberOpenID
	}
	if message.ID == "" || sender == "" || target.OpenID == "" {
		return logging.OperationError(ctx, "qq_reply", "invalid_message", "QQ message has no message ID or sender", nil)
	}
	content := randomAcknowledgement()
	code := strings.TrimSpace(message.Content)
	if strings.HasPrefix(code, "nqp_") {
		var err error
		content, err = s.bindingReply(ctx, target, code)
		if err != nil {
			return err
		}
	}
	_, err := s.qq.Reply(ctx, target, message.ID, content)
	if err != nil {
		// Binding may already be committed; a failed reply must not replay the event.
		replyError := logging.OperationError(ctx, "qq_reply", "qq_reply_failed", "QQ message reply could not be sent", err)
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "QQ message reply failed", slog.String("event", "qq_reply_failed"), logging.ErrorAttribute(replyError))
		return nil
	}
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "QQ message reply accepted", slog.String("event", "qq_reply_accepted"))
	return nil
}

func (s *service) bindingReply(ctx context.Context, target recipient.Target, code string) (string, error) {
	key, changed, err := s.keys.Bind(ctx, code, target)
	if err != nil {
		return "", logging.OperationError(ctx, "pairing", "pairing_failed", "Recipient binding could not be saved", err)
	}
	if !changed {
		return "Pairing code is invalid or expired. Please generate a new one.", nil
	}
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "Notification recipient paired", slog.String("event", "recipient_paired"), slog.String("api_key_id", key.ID), slog.String("recipient_type", string(target.Type)), slog.String("recipient_ref", recipientReference(target)))
	return "Paired successfully. Notifications are ready.", nil
}

func randomAcknowledgement() string {
	replies := [...]string{"Got it.", "Noted.", "Copy that.", "Received."}
	return replies[rand.IntN(len(replies))]
}
