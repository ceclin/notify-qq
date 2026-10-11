package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samber/oops"

	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/telemetry"
)

const apiBaseURL = "https://api.bot.qq.com"
const tokenURL = apiBaseURL + "/app/getAppAccessToken"

type Error struct {
	Status     int    `json:"http_status"`
	Code       int    `json:"code,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
	RetryAfter string `json:"-"`
}

func (e *Error) Error() string { return "QQ rejected the request" }

func (e *Error) RateLimited() bool {
	return e.Status == http.StatusTooManyRequests || e.Code == 100001 || e.Code == 40034100 || e.Code == 1100100
}

type Client struct {
	http             *http.Client
	telemetry        *telemetry.Runtime
	appID, secret    string
	tokenMutex       sync.Mutex
	cachedToken      string
	tokenExpiry      time.Time
	refreshCompleted chan struct{}
	gatewayConnected atomic.Bool
}

type Config struct {
	AppID, AppSecret string
}

type Option func(*Client)

func WithTelemetry(runtime *telemetry.Runtime) Option {
	return func(client *Client) { client.telemetry = runtime }
}

// Inject a transport for behavior tests or deployment-specific networking;
// nil keeps the bounded default client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(client *Client) {
		if httpClient != nil {
			client.http = httpClient
		}
	}
}

func NewClient(c Config, options ...Option) *Client {
	client := &Client{
		http:  &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		appID: c.AppID, secret: c.AppSecret,
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// Waiting callers retain their own deadlines; refreshing never holds the cache
// mutex across network I/O and doesn't leave a background goroutine behind.
func (q *Client) accessToken(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", logging.OperationError(ctx, "qq", "request_canceled", "QQ request was canceled", err)
		}
		q.tokenMutex.Lock()
		if q.cachedToken != "" && time.Now().Before(q.tokenExpiry) {
			token := q.cachedToken
			q.tokenMutex.Unlock()
			logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "Using cached QQ access token", slog.String("event", "qq_token_cache_hit"))
			return token, nil
		}
		if completed := q.refreshCompleted; completed != nil {
			q.tokenMutex.Unlock()
			logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "Waiting for QQ token refresh", slog.String("event", "qq_token_refresh_wait"))
			select {
			case <-completed:
				continue
			case <-ctx.Done():
				return "", logging.OperationError(ctx, "qq", "request_canceled", "QQ token wait was canceled", ctx.Err())
			}
		}
		q.refreshCompleted = make(chan struct{})
		q.tokenMutex.Unlock()
		started := time.Now()
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "Refreshing QQ access token", slog.String("event", "qq_token_refresh_started"))
		token, ttl, err := q.fetchToken(ctx)
		q.tokenMutex.Lock()
		if err == nil {
			q.cachedToken = token
			refreshMargin := min(60*time.Second, ttl/10)
			q.tokenExpiry = started.Add(ttl - refreshMargin)
		}
		close(q.refreshCompleted)
		q.refreshCompleted = nil
		q.tokenMutex.Unlock()
		if err == nil {
			logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "QQ access token refreshed",
				slog.String("event", "qq_token_refreshed"),
				slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))),
				slog.Float64("ttl_ms", logging.Milliseconds(ttl)),
			)
		}
		return token, err
	}
}

func (q *Client) fetchToken(ctx context.Context) (string, time.Duration, error) {
	data, err := q.request(ctx, http.MethodPost, "token", tokenURL, "", struct {
		AppID  string `json:"appId"`
		Secret string `json:"clientSecret"`
	}{q.appID, q.secret})
	if err != nil {
		return "", 0, err
	}
	var result struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", 0, logging.OperationError(ctx, "qq", "qq_invalid_response", "QQ token response is invalid", err)
	}
	// QQ's documented example uses a quoted expiry while its schema uses a number.
	seconds, err := strconv.ParseInt(string(bytes.Trim(result.ExpiresIn, `"`)), 10, 64)
	if err != nil || seconds <= 0 || seconds > 86400 || result.AccessToken == "" {
		return "", 0, logging.OperationError(ctx, "qq", "qq_invalid_response", "QQ token response has no valid token or expiry", nil)
	}
	return result.AccessToken, time.Duration(seconds) * time.Second, nil
}

type Message struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp,omitempty"`
}

func (q *Client) Send(ctx context.Context, userOpenID string, notification Notification) (Message, error) {
	token, err := q.accessToken(ctx)
	if err != nil {
		return Message{}, err
	}
	data, err := q.request(ctx, http.MethodPost, "send_message", apiBaseURL+"/v2/users/"+url.PathEscape(userOpenID)+"/messages", token, messagePayload(notification))
	if err != nil {
		var upstream *Error
		if errors.As(err, &upstream) && upstream.Status == http.StatusUnauthorized {
			q.invalidateToken(ctx, token)
		}
		// A transport error can occur after acceptance; automatic replay could send
		// the same notification twice. Callers decide whether to retry.
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "QQ send was not retried", slog.String("event", "qq_send_retry_skipped"), slog.String("reason", "duplicate_delivery_risk"))
		return Message{}, err
	}
	var result Message
	if err := json.Unmarshal(data, &result); err != nil {
		return result, logging.OperationError(ctx, "qq", "qq_invalid_response", "QQ send response is invalid", err)
	}
	if result.ID == "" {
		return result, logging.OperationError(ctx, "qq", "qq_invalid_response", "QQ send response has no message ID", nil)
	}
	return result, nil
}

func (q *Client) invalidateToken(ctx context.Context, token string) {
	q.tokenMutex.Lock()
	if q.cachedToken == token {
		q.cachedToken = ""
	}
	q.tokenMutex.Unlock()
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "QQ access token cache invalidated", slog.String("event", "qq_token_invalidated"))
}

func (q *Client) request(ctx context.Context, method, operation, endpoint, token string, payload any) (_ []byte, requestError error) {
	ctx, complete := q.telemetry.StartQQCall(ctx, method, operation)
	responseStatus := 0
	defer func() { complete(responseStatus, requestError != nil) }()
	builder := logging.WithErrorAttributes(oops.FromContext(ctx).In("qq"), slog.String("qq_operation", operation))
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return nil, builder.Code("qq_request_invalid").Wrapf(err, "QQ request could not be encoded")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, builder.Code("qq_request_invalid").Wrapf(err, "QQ request could not be created")
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		request.Header.Set("Authorization", "QQBot "+token)
	}
	started := time.Now()
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "QQ API request started",
		slog.String("event", "qq_request_started"),
		slog.String("qq_operation", operation),
		slog.String("host", request.URL.Host),
	)
	response, err := q.http.Do(request)
	if err != nil {
		// net/http wraps transport failures with the full URL, which contains the
		// recipient. Retain the transport cause without exposing that identifier.
		cause := err
		if urlError, ok := errors.AsType[*url.Error](err); ok {
			cause = urlError.Err
		}
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			cause = errors.Join(context.DeadlineExceeded, cause)
		}
		return nil, builder.Code("qq_transport_failed").Since(started).Wrapf(cause, "QQ request could not complete")
	}
	responseStatus = response.StatusCode
	// The response is read below; read failures own reporting, and closing a
	// read-only body must not replace the operation's result.
	defer func() { _ = response.Body.Close() }()
	const maxResponse = 1 << 20
	data, err = io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return nil, builder.Code("qq_response_read_failed").Wrapf(err, "QQ response could not be read")
	}
	if len(data) > maxResponse {
		return nil, builder.Code("qq_response_too_large").New("QQ response exceeded size limit")
	}
	var envelope struct {
		Code    int    `json:"code"`
		ErrCode int    `json:"err_code"`
		TraceID string `json:"trace_id"`
	}
	parseError := json.Unmarshal(data, &envelope)
	code := envelope.ErrCode
	if code == 0 {
		code = envelope.Code
	}
	trace := envelope.TraceID
	if trace == "" {
		trace = response.Header.Get("X-Tps-trace-ID")
	}
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "QQ API request completed",
		slog.String("event", "qq_request_completed"),
		slog.String("qq_operation", operation),
		slog.Int("qq_http_status", response.StatusCode),
		slog.Int("qq_code", code),
		slog.String("qq_trace_id", trace),
		slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))),
	)
	if response.StatusCode < 200 || response.StatusCode >= 300 || code != 0 {
		upstream := &Error{Status: response.StatusCode, Code: code, TraceID: trace, RetryAfter: response.Header.Get("Retry-After")}
		return nil, logging.WithErrorAttributes(builder.Code("qq_request_rejected"),
			slog.Int("qq_http_status", upstream.Status),
			slog.Int("qq_code", code),
			slog.String("qq_trace_id", trace),
		).Since(started).Wrap(upstream)
	}
	if parseError != nil {
		return nil, builder.Code("qq_invalid_response").Wrapf(parseError, "QQ response is invalid JSON")
	}
	return data, nil
}
