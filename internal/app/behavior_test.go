package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/labstack/echo/v5"

	"github.com/ceclin/notify-qq/internal/apikey"
	"github.com/ceclin/notify-qq/internal/config"
	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/qq"
	"github.com/ceclin/notify-qq/internal/telemetry"
	"github.com/ceclin/notify-qq/internal/testsupport"
)

type behaviorFixture struct {
	service      *service
	settings     config.Config
	logger       *slog.Logger
	apiKey       string
	pairingCode  string
	server       *httptest.Server
	upstream     *httptest.Server
	qqHTTP       *http.Client
	contract     *openapi3.T
	gatewayURL   string
	telemetry    *telemetry.Runtime
	tokenCalls   atomic.Int32
	sendCalls    atomic.Int32
	messageMutex sync.Mutex
	messages     []map[string]any
}

func startBehaviorService(t *testing.T, recipient string, upstreamBehavior http.HandlerFunc, additionalRoutes ...func(*echo.Echo)) *behaviorFixture {
	t.Helper()
	return startBehaviorServiceWithPrefix(t, recipient, upstreamBehavior, "", additionalRoutes...)
}

func startBehaviorServiceWithPrefix(t *testing.T, recipient string, upstreamBehavior http.HandlerFunc, prefix string, additionalRoutes ...func(*echo.Echo)) *behaviorFixture {
	return startBehaviorServiceWithPrefixAndLogger(t, recipient, upstreamBehavior, prefix, logging.NewLogger(io.Discard, slog.LevelInfo), additionalRoutes...)
}

func startBehaviorServiceWithLogger(t *testing.T, recipient string, upstreamBehavior http.HandlerFunc, logger *slog.Logger, additionalRoutes ...func(*echo.Echo)) *behaviorFixture {
	return startBehaviorServiceWithPrefixAndLogger(t, recipient, upstreamBehavior, "", logger, additionalRoutes...)
}

func startBehaviorServiceWithPrefixAndLogger(t *testing.T, recipient string, upstreamBehavior http.HandlerFunc, prefix string, logger *slog.Logger, additionalRoutes ...func(*echo.Echo)) *behaviorFixture {
	return startBehaviorServiceWithTelemetry(t, recipient, upstreamBehavior, prefix, logger, nil, additionalRoutes...)
}

func startBehaviorServiceWithTelemetry(t *testing.T, recipient string, upstreamBehavior http.HandlerFunc, prefix string, logger *slog.Logger, observability *telemetry.Runtime, additionalRoutes ...func(*echo.Echo)) *behaviorFixture {
	t.Helper()
	fixture := &behaviorFixture{contract: loadBehaviorContract(t), logger: logger, telemetry: observability}
	fixture.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/app/getAppAccessToken" {
			fixture.tokenCalls.Add(1)
			var credentials map[string]string
			_ = json.NewDecoder(r.Body).Decode(&credentials)
			if credentials["appId"] != "test-app" || credentials["clientSecret"] != "test-secret" {
				t.Error("QQ credential request did not use configured credentials")
			}
			if upstreamBehavior != nil {
				upstreamBehavior(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":"7200"}`)
			return
		}
		if r.URL.Path == "/gateway/bot" {
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "QQBot test-token" {
				t.Error("gateway discovery used incorrect method or authorization")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"url": fixture.gatewayURL})
			return
		}
		fixture.sendCalls.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/v2/users/") || !strings.HasSuffix(r.URL.Path, "/messages") || r.Header.Get("Authorization") != "QQBot test-token" {
			t.Error("QQ message used incorrect recipient or authorization")
		}
		var message map[string]any
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
		}
		message["recipient"] = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/users/"), "/messages")
		fixture.messageMutex.Lock()
		fixture.messages = append(fixture.messages, message)
		fixture.messageMutex.Unlock()
		if upstreamBehavior != nil {
			upstreamBehavior(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":"qq-message-id","timestamp":"2026-10-11T20:00:00+08:00"}`)
	}))
	settings := config.Config{
		AppID: "test-app", AppSecret: "test-secret",
		DataDir:       t.TempDir(),
		APIPathPrefix: prefix,
	}
	keys, err := apikey.Open(t.Context(), settings.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := keys.Create(t.Context(), "behavior-fixture")
	fixture.apiKey, fixture.pairingCode = created.APIKey, created.PairingCode
	if err == nil && recipient != "" {
		_, _, err = keys.Bind(t.Context(), fixture.pairingCode, recipient)
	}
	if err := errors.Join(err, keys.Close()); err != nil {
		t.Fatal(err)
	}
	fixture.qqHTTP = testsupport.QQClient(t, fixture.upstream)
	fixture.qqHTTP.Timeout = time.Second
	fixture.qqHTTP.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	fixture.settings = settings
	app := fixture.assemble(t)
	fixture.service = app
	routes := app.routes()
	for _, configureRoutes := range additionalRoutes {
		configureRoutes(routes)
	}
	fixture.server = httptest.NewServer(routes)
	t.Cleanup(func() {
		fixture.server.Close()
		fixture.upstream.Close()
		if err := fixture.service.keys.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

func (fixture *behaviorFixture) assemble(t *testing.T) *service {
	t.Helper()
	store, err := apikey.Open(t.Context(), fixture.settings.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	client := qq.NewClient(qq.Config{AppID: fixture.settings.AppID, AppSecret: fixture.settings.AppSecret}, qq.WithHTTPClient(fixture.qqHTTP), qq.WithTelemetry(fixture.telemetry))
	service := newService(fixture.settings.APIPathPrefix, fixture.logger, store, client, fixture.telemetry)
	service.running.Store(true)
	return service
}

func (fixture *behaviorFixture) request(t *testing.T, method, path, body string, headers map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, fixture.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if _, specified := headers["Content-Type"]; !specified && body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	requireBehaviorContract(t, fixture.contract, fixture.settings.APIPathPrefix, request, response, data, result)
	return response.StatusCode, result, response.Header
}

func (fixture *behaviorFixture) notify(t *testing.T, body string) (int, map[string]any, http.Header) {
	return fixture.request(t, http.MethodPost, fixture.settings.APIPathPrefix+"/notify", body, map[string]string{
		"Authorization": "Bearer " + fixture.apiKey, "Content-Type": "application/json",
	})
}

func requireStatus(t *testing.T, actual, expected int) {
	t.Helper()
	if actual != expected {
		t.Fatalf("HTTP status = %d, want %d", actual, expected)
	}
}

func TestBehaviorPairAndNotifyAcrossRestart(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	status, _, _ := fixture.notify(t, `{"content":"before pairing"}`)
	requireStatus(t, status, 409)
	if err := fixture.deliverMessage(t, "other-openid", "bind wrong-code"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "owner-openid", "bind "+fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "other-openid", "bind "+fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "owner-openid", "bind "+fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	status, result, _ := fixture.request(t, "GET", "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, 200)
	if result["paired"] != true || result["user_openid"] != "owner-openid" {
		t.Fatalf("unexpected pairing status: %v", result)
	}
	status, result, headers := fixture.notify(t, `{"content":"## Backup\n\nFinished ✓"}`)
	requireStatus(t, status, 200)
	if result["message_id"] != "qq-message-id" || result["status"] != "accepted" || headers.Get("X-Request-ID") == "" {
		t.Fatalf("unexpected notification result: %v", result)
	}
	if fixture.messages[0]["markdown"].(map[string]any)["content"] != "## Backup\n\nFinished ✓" || fixture.messages[0]["msg_type"] != float64(2) || fixture.messages[0]["msg_id"] != nil {
		t.Fatalf("incorrect proactive message: %v", fixture.messages[0])
	}
	fixture.server.Close()
	if err := fixture.service.keys.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := fixture.assemble(t)
	fixture.server = httptest.NewServer(restarted.routes())
	fixture.service = restarted
	status, _, _ = fixture.notify(t, `{"content":"after restart"}`)
	requireStatus(t, status, 200)
	fileInfo, err := os.Stat(filepath.Join(fixture.settings.DataDir, apikey.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Errorf("database file permissions = %o, want 600", fileInfo.Mode().Perm())
	}
	if fixture.sendCalls.Load() != 2 {
		t.Fatalf("unexpected number of QQ sends: %d", fixture.sendCalls.Load())
	}
}

func TestBehaviorWebhookIsNotExposed(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	status, _, _ := fixture.request(t, http.MethodPost, "/qq/webhook", `{"op":0,"t":"C2C_MESSAGE_CREATE"}`, nil)
	requireStatus(t, status, http.StatusNotFound)
	key, valid, err := fixture.service.keys.Authenticate(t.Context(), fixture.apiKey)
	if err != nil || !valid {
		t.Fatal("fixture key could not be read")
	}
	if key.UserOpenID != "" || fixture.tokenCalls.Load() != 0 {
		t.Fatal("removed webhook altered pairing or contacted QQ")
	}
}

func TestBehaviorRejectInvalidNotificationsWithoutContactingQQ(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", nil)
	for _, scenario := range []struct {
		name, body, key, mediaType string
		status                     int
	}{
		{"missing key", `{"content":"hello"}`, "", "application/json", 401},
		{"wrong key", `{"content":"hello"}`, "wrong", "application/json", 401},
		{"wrong media type", `{"content":"hello"}`, fixture.apiKey, "text/plain", 415},
		{"malformed JSON", `{`, fixture.apiKey, "application/json", 400},
		{"empty message", `{"content":" "}`, fixture.apiKey, "application/json", 400},
		{"recipient override", `{"content":"hello","user_openid":"other"}`, fixture.apiKey, "application/json", 400},
		{"multiple objects", `{"content":"hello"}{}`, fixture.apiKey, "application/json", 400},
		{"large message", `{"content":"` + strings.Repeat("x", 4001) + `"}`, fixture.apiKey, "application/json", 400},
		{"large body", `{"content":"` + strings.Repeat("x", maxRequestBytes) + `"}`, fixture.apiKey, "application/json", 413},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			status, result, headers := fixture.request(t, "POST", "/notify", scenario.body, map[string]string{"Authorization": "Bearer " + scenario.key, "Content-Type": scenario.mediaType})
			requireStatus(t, status, scenario.status)
			if result["error"] == nil || result["request_id"] != headers.Get("X-Request-ID") {
				t.Fatal("error response is missing correlation fields")
			}
		})
	}
	if fixture.sendCalls.Load() != 0 || fixture.tokenCalls.Load() != 0 {
		t.Fatal("invalid requests contacted QQ")
	}
}

func TestBehaviorRoutingErrorsPreserveStatusAndCorrelation(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", nil)
	for _, scenario := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/missing", http.StatusNotFound},
		{http.MethodGet, "/notify", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
	} {
		t.Run(scenario.method+" "+scenario.path, func(t *testing.T) {
			status, result, headers := fixture.request(t, scenario.method, scenario.path, "", nil)
			requireStatus(t, status, scenario.status)
			failure := result["error"].(map[string]any)
			if failure["code"] != "request_rejected" || failure["message"] != http.StatusText(scenario.status) {
				t.Fatalf("incorrect routing failure: %v", result)
			}
			if headers.Get("X-Request-ID") == "" || result["request_id"] != headers.Get("X-Request-ID") {
				t.Fatal("routing failure lost request correlation")
			}
			if scenario.status == http.StatusMethodNotAllowed && headers.Get("Allow") == "" {
				t.Fatal("method rejection has no Allow header")
			}
		})
	}
	if fixture.sendCalls.Load() != 0 || fixture.tokenCalls.Load() != 0 {
		t.Fatal("routing failures contacted QQ")
	}
}

func TestBehaviorQQFailuresAreVisibleAndNeverReplayed(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		tokenFailure bool
		httpStatus   int
		body         string
		expected     int
	}{
		{"business error at HTTP 200", false, 200, `{"err_code":40034105,"message":"no permission","trace_id":"trace"}`, 502},
		{"code business error", false, 200, `{"code":40054013,"message":"rejected"}`, 502},
		{"rate limit at HTTP 200", false, 200, `{"err_code":40034100}`, 429},
		{"HTTP rate limit", false, 429, `{"code":100001}`, 429},
		{"unauthorized", false, 401, `{"code":11242}`, 502},
		{"missing message ID", false, 200, `{}`, 502},
		{"malformed response", false, 200, `not json`, 502},
		{"token business error at HTTP 200", true, 200, `{"code":100016,"message":"secret invalid"}`, 502},
		{"invalid token expiry", true, 200, `{"access_token":"token","expires_in":"invalid"}`, 502},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := startBehaviorService(t, "owner-openid", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/app/getAppAccessToken" && !scenario.tokenFailure {
					_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
					return
				}
				w.WriteHeader(scenario.httpStatus)
				_, _ = io.WriteString(w, scenario.body)
			})
			status, result, headers := fixture.notify(t, `{"content":"hello"}`)
			requireStatus(t, status, scenario.expected)
			if result["error"] == nil {
				t.Fatal("QQ failure was reported as success")
			}
			if scenario.expected == 429 && headers.Get("Retry-After") == "" {
				t.Fatal("rate limit has no retry guidance")
			}
			expectedSends := int32(1)
			if scenario.tokenFailure {
				expectedSends = 0
			}
			if fixture.sendCalls.Load() != expectedSends || fixture.tokenCalls.Load() != 1 {
				t.Fatal("QQ failure caused an automatic replay")
			}
		})
	}
}

func TestBehaviorClientDisconnectCancelsQQRequest(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	fixture := startBehaviorService(t, "owner-openid", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-t.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.server.URL+"/notify", strings.NewReader(`{"content":"cancel this notification"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fixture.apiKey)
	request.Header.Set("Content-Type", "application/json")
	result := make(chan error, 1)
	go func() {
		response, err := fixture.server.Client().Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		result <- err
	}()
	deadline, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	select {
	case <-started:
	case <-deadline.Done():
		t.Fatal("notification did not reach QQ")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client cancellation = %v", err)
		}
	case <-deadline.Done():
		t.Fatal("client request did not stop")
	}
	select {
	case <-canceled:
	case <-deadline.Done():
		t.Fatal("client cancellation did not reach QQ")
	}
	if fixture.sendCalls.Load() != 1 {
		t.Fatal("canceled request was replayed")
	}
}

func TestBehaviorPanicRecoveryKeepsResponsePrivate(t *testing.T) {
	fixture := startBehaviorService(t, "", nil, func(app *echo.Echo) {
		app.GET("/panic", func(_ *echo.Context) error { panic("private diagnostic") })
	})
	status, result, headers := fixture.request(t, http.MethodGet, "/panic", "", nil)
	requireStatus(t, status, http.StatusInternalServerError)
	failure := result["error"].(map[string]any)
	if failure["code"] != "panic_recovered" || failure["message"] != "Internal server error" || result["request_id"] != headers.Get("X-Request-ID") {
		t.Fatalf("panic response did not preserve the public error contract: %v", result)
	}
	status, _, _ = fixture.request(t, http.MethodGet, "/healthz", "", nil)
	requireStatus(t, status, http.StatusOK)
}

func TestBehaviorConcurrentNotificationsShareTokenRefresh(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", nil)
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			status, _, _ := fixture.notify(t, `{"content":"concurrent notification"}`)
			if status != 200 {
				t.Errorf("notification status = %d, want 200", status)
			}
		})
	}
	workers.Wait()
	if fixture.tokenCalls.Load() != 1 || fixture.sendCalls.Load() != 12 {
		t.Fatalf("token calls = %d, sends = %d", fixture.tokenCalls.Load(), fixture.sendCalls.Load())
	}
}

func TestBehaviorQQTimeoutDoesNotReplay(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
			return
		}
		<-r.Context().Done()
	})
	fixture.qqHTTP.Timeout = 50 * time.Millisecond
	status, result, _ := fixture.notify(t, `{"content":"timeout"}`)
	requireStatus(t, status, 504)
	if fixture.sendCalls.Load() != 1 || result["error"] == nil {
		t.Fatal("timeout did not preserve uncertain delivery outcome")
	}
}

func TestBehaviorOopsLogsContainWorkflowAndNoSecrets(t *testing.T) {
	var captured bytes.Buffer
	// This behavior scenario runs without parallel tests so the process-wide
	// logger can be captured safely for assertions on its observable output.
	logger := logging.NewLogger(&captured, slog.LevelDebug)
	fixture := startBehaviorServiceWithLogger(t, "owner-openid", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
			return
		}
		_, _ = io.WriteString(w, `{"err_code":40034105,"trace_id":"qq-trace"}`)
	}, logger)
	status, _, _ := fixture.notify(t, `{"content":"private-notification-body"}`)
	requireStatus(t, status, 502)
	fixture.server.Close()
	logs := captured.String()
	for _, forbidden := range []string{fixture.apiKey, fixture.settings.AppSecret, fixture.pairingCode, "test-token", "private-notification-body"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("logs include a secret or notification body: %q", forbidden)
		}
	}
	var errorEvents int
	decoder := json.NewDecoder(strings.NewReader(logs))
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["event"] == "http_request_failed" {
			errorEvents++
			detailed, ok := entry["error"].(map[string]any)
			if entry["request_id"] == nil || !ok || detailed["stacktrace"] == nil || detailed["context"] == nil {
				t.Fatal("boundary error log lost correlation or Oops stack")
			}
			errorContext, ok := detailed["context"].(map[string]any)
			if !ok || errorContext["request_id"] != entry["request_id"] || errorContext["route"] != "/notify" ||
				errorContext["qq_operation"] != "send_message" || errorContext["qq_http_status"] != float64(200) ||
				errorContext["qq_code"] != float64(40034105) || errorContext["qq_trace_id"] != "qq-trace" {
				t.Fatal("typed error attributes lost request or QQ failure metadata")
			}
		}
	}
	if errorEvents != 1 {
		t.Fatalf("error was logged %d times, want once", errorEvents)
	}
}

func TestBehaviorUnauthorizedQQTokenRefreshesOnNextNotification(t *testing.T) {
	var sendSequence atomic.Int32
	fixture := startBehaviorService(t, "owner-openid", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
			return
		}
		if sendSequence.Add(1) == 1 {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"code":11242}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"qq-message-id"}`)
	})
	status, _, _ := fixture.notify(t, `{"content":"first attempt"}`)
	requireStatus(t, status, 502)
	if fixture.sendCalls.Load() != 1 {
		t.Fatal("unauthorized send was replayed")
	}
	status, _, _ = fixture.notify(t, `{"content":"next notification"}`)
	requireStatus(t, status, 200)
	if fixture.tokenCalls.Load() != 2 || fixture.sendCalls.Load() != 2 {
		t.Fatal("next notification did not refresh the invalid token")
	}
}

func TestBehaviorPairingStorageFailureLeavesRecipientUnbound(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	database, err := sql.Open("sqlite", filepath.Join(fixture.settings.DataDir, apikey.Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := database.ExecContext(t.Context(), "ALTER TABLE api_keys RENAME TO unavailable_keys"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "owner-openid", "bind "+fixture.pairingCode); err == nil {
		t.Fatal("failed binding storage reported success")
	}
	if _, err := database.ExecContext(t.Context(), "ALTER TABLE unavailable_keys RENAME TO api_keys"); err != nil {
		t.Fatal(err)
	}
	status, result, _ := fixture.request(t, "GET", "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, 200)
	if result["paired"] != false {
		t.Fatal("failed persistence changed recipient")
	}
	if err := fixture.deliverMessage(t, "owner-openid", "bind "+fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	status, _, _ = fixture.notify(t, `{"content":"after storage repair"}`)
	requireStatus(t, status, 200)
}

func TestBehaviorSlogSharesEchoAndDownstreamRequestFields(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var captured bytes.Buffer
			logger := logging.NewLogger(&captured, level)
			fixture := startBehaviorServiceWithLogger(t, "owner-openid", nil, logger, func(routes *echo.Echo) {
				routes.GET("/log-probe", func(c *echo.Context) error {
					c.Logger().LogAttrs(c.Request().Context(), slog.LevelInfo, "Echo logging probe", slog.String("event", "echo_logging_probe"))
					logging.FromContext(c.Request().Context()).LogAttrs(c.Request().Context(), slog.LevelInfo, "Context logging probe", slog.String("event", "context_logging_probe"))
					return c.JSON(200, map[string]bool{"ok": true})
				})
			})
			status, _, probeHeaders := fixture.request(t, "GET", "/log-probe", "", nil)
			requireStatus(t, status, 200)
			status, _, notificationHeaders := fixture.notify(t, `{"content":"private successful notification"}`)
			requireStatus(t, status, 200)
			status, _, _ = fixture.request(t, "GET", "/healthz", "", nil)
			requireStatus(t, status, 200)
			fixture.server.Close()
			observed := map[string]int{}
			decoder := json.NewDecoder(bytes.NewReader(captured.Bytes()))
			for decoder.More() {
				var entry map[string]any
				if err := decoder.Decode(&entry); err != nil {
					t.Fatal(err)
				}
				event, _ := entry["event"].(string)
				observed[event]++
				if entry["service"] != "notify-qq" || entry["msg"] == nil || entry["time"] == nil || entry["!BADKEY"] != nil {
					t.Fatalf("invalid structured log envelope: %v", entry)
				}
				if entry["route"] == "/log-probe" && (entry["request_id"] != probeHeaders.Get("X-Request-ID") || entry["method"] != "GET") {
					t.Fatal("Echo and context loggers lost request correlation")
				}
				if entry["route"] == "/notify" && (entry["request_id"] != notificationHeaders.Get("X-Request-ID") || entry["method"] != "POST") {
					t.Fatal("downstream logger lost request correlation")
				}
				for _, field := range []string{"duration_ms", "ttl_ms"} {
					if value, found := entry[field]; found {
						if _, ok := value.(float64); !ok {
							t.Fatalf("%s is no longer numeric milliseconds", field)
						}
					}
				}
				if level == slog.LevelInfo && (entry["level"] == "DEBUG" || entry["route"] == "/healthz") {
					t.Fatal("INFO logger emitted diagnostic or liveness noise")
				}
			}
			for _, event := range []string{"echo_logging_probe", "context_logging_probe", "notification_started", "notification_accepted", "qq_token_refreshed"} {
				if observed[event] != 1 {
					t.Fatalf("event %s occurred %d times, want once", event, observed[event])
				}
			}
			if level == slog.LevelDebug && (observed["qq_request_started"] != 2 || observed["qq_request_completed"] != 2) {
				t.Fatal("DEBUG logger lost QQ interaction diagnostics")
			}
			if strings.Contains(captured.String(), "private successful notification") {
				t.Fatal("successful notification text appeared in logs")
			}
		})
	}
}

func TestBehaviorSlogRateLimitUsesWarningLevel(t *testing.T) {
	var captured bytes.Buffer
	logger := logging.NewLogger(&captured, slog.LevelWarn)
	fixture := startBehaviorServiceWithLogger(t, "owner-openid", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
			return
		}
		_, _ = io.WriteString(w, `{"err_code":40034100}`)
	}, logger)
	status, _, headers := fixture.notify(t, `{"content":"rate limited"}`)
	requireStatus(t, status, 429)
	fixture.server.Close()
	decoder := json.NewDecoder(bytes.NewReader(captured.Bytes()))
	var entry map[string]any
	if err := decoder.Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "WARN" || entry["event"] != "http_request_failed" || entry["request_id"] != headers.Get("X-Request-ID") {
		t.Fatalf("unexpected warning log: %v", entry)
	}
	if decoder.More() {
		t.Fatal("rate limit was logged more than once or bypassed WARN filtering")
	}
}
