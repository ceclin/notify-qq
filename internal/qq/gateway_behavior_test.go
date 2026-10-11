package qq_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/qq"
	"github.com/ceclin/notify-qq/internal/testsupport"
)

type synchronizedLogs struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *synchronizedLogs) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(data)
}

func (l *synchronizedLogs) snapshot() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

type gatewayFixture struct {
	client      *qq.Client
	server      *httptest.Server
	logs        synchronizedLogs
	connections atomic.Int32
	tokens      atomic.Int32
	discoveries atomic.Int32
	discover    func(http.ResponseWriter, *http.Request)
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan error
	waited      bool
}

func newGatewayFixture(t *testing.T, serve func(context.Context, *websocket.Conn, int)) *gatewayFixture {
	t.Helper()
	fixture := &gatewayFixture{}
	fixture.ctx, fixture.cancel = context.WithTimeout(t.Context(), 8*time.Second)
	fixture.done = make(chan error, 1)
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			fixture.tokens.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test-token", "expires_in": 7200})
		case "/gateway/bot":
			fixture.discoveries.Add(1)
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "QQBot test-token" {
				t.Error("discovery used incorrect method or token")
			}
			if fixture.discover != nil {
				fixture.discover(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "wss://" + r.Host + "/socket"})
		case "/socket":
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = connection.CloseNow() }()
			serve(fixture.ctx, connection, int(fixture.connections.Add(1)))
		default:
			t.Error("unexpected QQ request path")
		}
	}))
	httpClient := testsupport.QQClient(t, fixture.server)
	httpClient.Timeout = time.Second
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	fixture.client = qq.NewClient(qq.Config{AppID: "test-app", AppSecret: "test-secret"}, qq.WithHTTPClient(httpClient))
	t.Cleanup(func() {
		fixture.cancel()
		if !fixture.waited {
			if err := fixture.wait(t); err != nil {
				t.Error(err)
			}
		}
		fixture.server.Close()
	})
	return fixture
}

func (f *gatewayFixture) start(handle func(context.Context, qq.Event) error) {
	ctx := logging.WithLogger(f.ctx, logging.NewLogger(&f.logs, slog.LevelDebug))
	go func() { f.done <- f.client.Listen(ctx, handle) }()
}

func (f *gatewayFixture) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.done:
		f.waited = true
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("gateway worker did not stop after cancellation")
		return nil
	}
}

type authentication struct {
	Opcode int `json:"op"`
	Data   struct {
		Token     string `json:"token"`
		Intents   int    `json:"intents"`
		Shard     [2]int `json:"shard"`
		SessionID string `json:"session_id"`
		Sequence  int64  `json:"seq"`
	} `json:"d"`
}

func gatewayAuthenticate(t *testing.T, ctx context.Context, connection *websocket.Conn, expected int, sequence int64) bool {
	t.Helper()
	if wsjson.Write(ctx, connection, map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 150}}) != nil {
		return false
	}
	var payload authentication
	if wsjson.Read(ctx, connection, &payload) != nil {
		return false
	}
	if payload.Opcode != expected || payload.Data.Token != "QQBot test-token" {
		t.Error("incorrect authentication opcode or token")
		return false
	}
	if expected == 2 && (payload.Data.Intents != 1<<25 || payload.Data.Shard != [2]int{0, 1}) {
		t.Error("Identify did not select C2C and a single shard")
		return false
	}
	if expected == 6 && (payload.Data.SessionID != "test-session" || payload.Data.Sequence != sequence) {
		t.Errorf("Resume lost the committed cursor: session matches=%v, sequence=%d, want %d", payload.Data.SessionID == "test-session", payload.Data.Sequence, sequence)
		return false
	}
	return true
}

func gatewayReady(ctx context.Context, connection *websocket.Conn) bool {
	return wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]string{"session_id": "test-session"}}) == nil
}

func gatewayEvent(ctx context.Context, connection *websocket.Conn, sequence int64) bool {
	return wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": sequence, "t": "C2C_MESSAGE_CREATE", "id": "event-id", "d": map[string]string{"content": "private message body"}}) == nil
}

func gatewayHeartbeat(ctx context.Context, connection *websocket.Conn, acknowledge bool) (int64, bool) {
	var frame struct {
		Opcode   int    `json:"op"`
		Sequence *int64 `json:"d"`
	}
	if wsjson.Read(ctx, connection, &frame) != nil || frame.Opcode != 1 {
		return 0, false
	}
	if acknowledge && wsjson.Write(ctx, connection, map[string]int{"op": 11}) != nil {
		return 0, false
	}
	if frame.Sequence == nil {
		return -1, true
	}
	return *frame.Sequence, true
}

func waitGatewaySignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("gateway behavior did not complete before its deadline")
	}
}

func TestBehaviorGatewayHeartbeatsContinueDuringSlowEventProcessing(t *testing.T) {
	heartbeats := make(chan struct{})
	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, attempt int) {
		if attempt != 1 || !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) || !gatewayEvent(ctx, connection, 2) {
			return
		}
		for range 8 {
			sequence, ok := gatewayHeartbeat(ctx, connection, true)
			if !ok {
				return
			}
			if sequence != 1 {
				t.Error("unfinished event was committed in a heartbeat")
				return
			}
		}
		close(heartbeats)
		for {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
	})
	fixture.start(func(ctx context.Context, event qq.Event) error {
		if event.Sequence != 2 {
			t.Error("unexpected event sequence")
		}
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	waitGatewaySignal(t, fixture.ctx, started)
	waitGatewaySignal(t, fixture.ctx, heartbeats)
	if !fixture.client.GatewayConnected() || fixture.connections.Load() != 1 {
		t.Fatal("slow event processing interrupted the connection")
	}
	releaseOnce()
}

func TestBehaviorGatewayResumePreservesQueuedEventsAndSkipsDuplicates(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	resumed := make(chan struct{})
	completed := make(chan struct{})
	var seen sync.Map
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, attempt int) {
		if attempt == 1 {
			if !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) || !gatewayEvent(ctx, connection, 2) {
				return
			}
			for {
				sequence, ok := gatewayHeartbeat(ctx, connection, true)
				if !ok {
					return
				}
				if sequence == 2 {
					_ = wsjson.Write(ctx, connection, map[string]int{"op": 7})
					return
				}
			}
		}
		if attempt != 2 || !gatewayAuthenticate(t, ctx, connection, 6, 2) || !gatewayEvent(ctx, connection, 3) || !gatewayEvent(ctx, connection, 4) {
			return
		}
		if wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 5, "t": "RESUMED", "d": ""}) != nil {
			return
		}
		sequence, ok := gatewayHeartbeat(ctx, connection, true)
		if !ok {
			return
		}
		if sequence != 2 {
			t.Error("RESUMED advanced the cursor past queued events")
			return
		}
		close(resumed)
		for {
			sequence, ok := gatewayHeartbeat(ctx, connection, true)
			if !ok {
				return
			}
			if sequence == 5 {
				break
			}
		}
		if !gatewayEvent(ctx, connection, 3) {
			return
		}
		for range 2 {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
		close(completed)
		for {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
	})
	fixture.start(func(ctx context.Context, event qq.Event) error {
		if event.Sequence == 3 {
			close(blocked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if _, duplicate := seen.LoadOrStore(event.Sequence, true); duplicate {
			t.Error("replayed event was processed twice")
		}
		return nil
	})
	waitGatewaySignal(t, fixture.ctx, blocked)
	waitGatewaySignal(t, fixture.ctx, resumed)
	releaseOnce()
	waitGatewaySignal(t, fixture.ctx, completed)
	for _, sequence := range []int64{2, 3, 4} {
		if _, found := seen.Load(sequence); !found {
			t.Errorf("event %d was skipped", sequence)
		}
	}
	for _, secret := range []string{"test-token", "test-secret", "private message body", "test-session"} {
		if strings.Contains(fixture.logs.snapshot(), secret) {
			t.Fatalf("gateway logs leaked %q", secret)
		}
	}
}

func TestBehaviorGatewaySessionRecoveryPolicies(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		closeCode      websocket.StatusCode
		invalidSession bool
		nextOpcode     int
		tokenCalls     int32
	}{
		{"invalid session", 0, true, 2, 2}, {"expired session", 4006, false, 2, 1}, {"expired connection", 4009, false, 6, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			recovered := make(chan struct{})
			fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, attempt int) {
				if attempt == 1 {
					if !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) {
						return
					}
					if scenario.invalidSession {
						_ = wsjson.Write(ctx, connection, map[string]any{"op": 9, "d": false})
					} else {
						_ = connection.Close(scenario.closeCode, "private message body test-secret")
					}
					return
				}
				if attempt != 2 || !gatewayAuthenticate(t, ctx, connection, scenario.nextOpcode, 1) {
					return
				}
				if scenario.nextOpcode == 6 {
					if wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 2, "t": "RESUMED", "d": ""}) != nil {
						return
					}
				} else if !gatewayReady(ctx, connection) {
					return
				}
				close(recovered)
				for {
					if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
						return
					}
				}
			})
			fixture.start(nil)
			waitGatewaySignal(t, fixture.ctx, recovered)
			if fixture.tokens.Load() != scenario.tokenCalls {
				t.Errorf("token refresh count=%d, want %d", fixture.tokens.Load(), scenario.tokenCalls)
			}
			if strings.Contains(fixture.logs.snapshot(), "private message body") || strings.Contains(fixture.logs.snapshot(), "test-secret") {
				t.Fatal("server close reason leaked into logs")
			}
		})
	}
}

func TestBehaviorGatewayMissingHeartbeatAckReconnects(t *testing.T) {
	recovered := make(chan struct{})
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, attempt int) {
		if attempt == 1 {
			if !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) {
				return
			}
			for {
				if _, ok := gatewayHeartbeat(ctx, connection, false); !ok {
					return
				}
			}
		}
		if attempt != 2 || !gatewayAuthenticate(t, ctx, connection, 6, 1) {
			return
		}
		if wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 2, "t": "RESUMED", "d": ""}) != nil {
			return
		}
		close(recovered)
		for {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
	})
	fixture.start(nil)
	waitGatewaySignal(t, fixture.ctx, recovered)
	if !strings.Contains(fixture.logs.snapshot(), "gateway_heartbeat_timeout") {
		t.Fatal("heartbeat failure was not reported")
	}
}

func TestBehaviorGatewayTerminalPermissionFailureStopsWithoutRetry(t *testing.T) {
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, _ int) {
		if gatewayAuthenticate(t, ctx, connection, 2, 0) {
			_ = connection.Close(4014, "test-token test-secret private message body")
		}
	})
	fixture.start(nil)
	if err := fixture.wait(t); err == nil {
		t.Fatal("permission denial did not stop the worker")
	}
	if fixture.connections.Load() != 1 || fixture.client.GatewayConnected() {
		t.Fatal("terminal failure was retried or remained connected")
	}
	logs := fixture.logs.snapshot()
	if strings.Count(logs, `"event":"qq_gateway_failed"`) != 1 || strings.Contains(logs, `"event":"qq_gateway_retry"`) {
		t.Fatal("terminal error was not reported exactly once")
	}
	for _, secret := range []string{"test-token", "test-secret", "private message body"} {
		if strings.Contains(logs, secret) {
			t.Fatal("terminal close reason leaked")
		}
	}
}

func TestBehaviorGatewayRejectsUnsafeDiscoveryAndMalformedFrames(t *testing.T) {
	for _, scenario := range []string{"insecure gateway URL", "binary frame", "invalid Hello"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, _ int) {
				if scenario == "invalid Hello" {
					_ = wsjson.Write(ctx, connection, map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 0}})
					var discard any
					_ = wsjson.Read(ctx, connection, &discard)
					return
				}
				if gatewayAuthenticate(t, ctx, connection, 2, 0) {
					_ = connection.Write(ctx, websocket.MessageBinary, []byte("test-token private message body"))
					var discard any
					_ = wsjson.Read(ctx, connection, &discard)
				}
			})
			if scenario == "insecure gateway URL" {
				fixture.discover = func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://example.invalid/socket"})
				}
			}
			fixture.start(nil)
			waitGatewayLog(t, fixture, `"event":"qq_gateway_retry"`)
			if fixture.client.GatewayConnected() {
				t.Fatal("invalid gateway became connected")
			}
			if scenario == "insecure gateway URL" && fixture.connections.Load() != 0 {
				t.Fatal("unsafe gateway was contacted")
			}
			if strings.Contains(fixture.logs.snapshot(), "private message body") || strings.Contains(fixture.logs.snapshot(), "test-token") {
				t.Fatal("malformed frame leaked into logs")
			}
		})
	}
}

func TestBehaviorGatewayDiscoveryUnauthorizedRefreshesToken(t *testing.T) {
	connected := make(chan struct{})
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, _ int) {
		if !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) {
			return
		}
		close(connected)
		for {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
	})
	fixture.discover = func(w http.ResponseWriter, r *http.Request) {
		if fixture.discoveries.Load() == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]int{"code": 11242})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "wss://" + r.Host + "/socket"})
	}
	fixture.start(nil)
	waitGatewaySignal(t, fixture.ctx, connected)
	if fixture.tokens.Load() != 2 || fixture.connections.Load() != 1 {
		t.Fatal("gateway authorization failure did not refresh the token for the next attempt")
	}
}

func waitGatewayLog(t *testing.T, fixture *gatewayFixture, pattern string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !strings.Contains(fixture.logs.snapshot(), pattern) {
		select {
		case <-ticker.C:
		case <-fixture.ctx.Done():
			t.Fatal("expected gateway log was not emitted")
		}
	}
}

func TestBehaviorGatewayFullQueueResumesFromProcessedCursor(t *testing.T) {
	replayed := make(chan struct{})
	handlerStarted := make(chan struct{})
	var calls atomic.Int32
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, attempt int) {
		if attempt == 1 {
			if !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) || !gatewayEvent(ctx, connection, 2) {
				return
			}
			select {
			case <-handlerStarted:
			case <-ctx.Done():
				return
			}
			for sequence := int64(3); sequence < 40; sequence++ {
				if !gatewayEvent(ctx, connection, sequence) {
					return
				}
			}
			return
		}
		if attempt != 2 || !gatewayAuthenticate(t, ctx, connection, 6, 1) || !gatewayEvent(ctx, connection, 2) {
			return
		}
		if wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 3, "t": "RESUMED", "d": ""}) != nil {
			return
		}
		for {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
	})
	fixture.start(func(ctx context.Context, event qq.Event) error {
		if calls.Add(1) == 1 {
			close(handlerStarted)
			<-ctx.Done()
			return ctx.Err()
		}
		if event.Sequence != 2 {
			t.Error("unprocessed event was skipped on resume")
		}
		close(replayed)
		return nil
	})
	waitGatewaySignal(t, fixture.ctx, replayed)
	if !strings.Contains(fixture.logs.snapshot(), "gateway_queue_full") {
		t.Fatal("queue overflow was not reported")
	}
}

func TestBehaviorGatewaySessionStartQuotaDelaysIdentify(t *testing.T) {
	connected := make(chan struct{})
	var firstDiscovery atomic.Int64
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, _ int) {
		if time.Since(time.Unix(0, firstDiscovery.Load())) < 1500*time.Millisecond {
			t.Error("session quota reset was not respected")
		}
		if !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) {
			return
		}
		close(connected)
		for {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
	})
	fixture.discover = func(w http.ResponseWriter, r *http.Request) {
		remaining := 1
		if fixture.discoveries.Load() == 1 {
			firstDiscovery.Store(time.Now().UnixNano())
			remaining = 0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "wss://" + r.Host + "/socket", "session_start_limit": map[string]int{"remaining": remaining, "reset_after": 1500}})
	}
	fixture.start(nil)
	waitGatewaySignal(t, fixture.ctx, connected)
	if fixture.connections.Load() != 1 || !strings.Contains(fixture.logs.snapshot(), "gateway_session_limit") {
		t.Fatal("exhausted session quota did not defer the socket connection")
	}
}

func TestBehaviorGatewayDiscoveryRateLimitHonorsRetryAfter(t *testing.T) {
	connected := make(chan struct{})
	var limitedAt atomic.Int64
	fixture := newGatewayFixture(t, func(ctx context.Context, connection *websocket.Conn, _ int) {
		if time.Since(time.Unix(0, limitedAt.Load())) < 1500*time.Millisecond {
			t.Error("gateway Retry-After was not respected")
		}
		if !gatewayAuthenticate(t, ctx, connection, 2, 0) || !gatewayReady(ctx, connection) {
			return
		}
		close(connected)
		for {
			if _, ok := gatewayHeartbeat(ctx, connection, true); !ok {
				return
			}
		}
	})
	fixture.discover = func(w http.ResponseWriter, r *http.Request) {
		if fixture.discoveries.Load() == 1 {
			limitedAt.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]int{"code": 100001})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "wss://" + r.Host + "/socket"})
	}
	fixture.start(nil)
	waitGatewaySignal(t, fixture.ctx, connected)
	if fixture.discoveries.Load() != 2 || fixture.connections.Load() != 1 {
		t.Fatal("gateway rate limit caused extra requests")
	}
}
