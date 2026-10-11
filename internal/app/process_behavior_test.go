package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/ceclin/notify-qq/internal/apikey"
)

// Build and launch the real entry point so exit codes, configuration failures,
// signal handling, and request draining stay covered when this template evolves.
func TestBehaviorProcessLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process behavior requires POSIX signals")
	}
	binary := filepath.Join(t.TempDir(), "notify-qq")
	buildCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-race", "-o", binary, "./cmd/notify-qq")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build service: %v\n%s", err, output)
	}

	t.Run("local API key management", func(t *testing.T) { testProcessAPIKeys(t, binary) })
	t.Run("OpenTelemetry lifecycle", func(t *testing.T) { testProcessTelemetry(t, binary) })
	t.Run("version reports build metadata without configuration or storage", func(t *testing.T) {
		versioned := filepath.Join(t.TempDir(), "notify-qq-versioned")
		flags := "-X github.com/ceclin/notify-qq/internal/app.version=v0.1.0-preview.1 -X github.com/ceclin/notify-qq/internal/app.commit=0123456789abcdef -X github.com/ceclin/notify-qq/internal/app.builtAt=2026-10-14T00:00:00Z"
		build := exec.CommandContext(t.Context(), "go", "build", "-ldflags", flags, "-o", versioned, "./cmd/notify-qq")
		build.Dir = filepath.Join("..", "..")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build versioned service: %v\n%s", err, output)
		}
		for _, scenario := range []struct {
			binary, version, commit, builtAt string
		}{
			{binary, "dev", "unknown", "unknown"},
			{versioned, "v0.1.0-preview.1", "0123456789abcdef", "2026-10-14T00:00:00Z"},
		} {
			directory := t.TempDir()
			dataDir := filepath.Join(directory, "uncreated")
			command := exec.CommandContext(t.Context(), scenario.binary, "--version")
			command.Dir = directory
			command.Env = append(os.Environ(), "LOG_LEVEL=invalid", "QQ_APP_ID=", "QQ_APP_SECRET=", "DATA_DIR="+dataDir, "GORACE=atexit_sleep_ms=0")
			output, err := command.Output()
			if err != nil {
				t.Fatalf("version command failed with invalid server configuration: %v", err)
			}
			var metadata map[string]string
			if err := json.Unmarshal(output, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["version"] != scenario.version || metadata["commit"] != scenario.commit || metadata["built_at"] != scenario.builtAt || !strings.HasPrefix(metadata["go_version"], "go") {
				t.Fatalf("incorrect build metadata: %v", metadata)
			}
			if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("version command opened application storage")
			}
		}
	})

	t.Run("invalid configuration exits once", func(t *testing.T) {
		process := launchServiceProcess(t, binary, map[string]string{"LOG_LEVEL": "invalid"})
		process.requireStartupFailure(t)
	})
	t.Run("removed QQ endpoint overrides have no effect", func(t *testing.T) {
		process := launchServiceProcess(t, binary, map[string]string{
			"QQ_API_BASE_URL": "https://unsupported.invalid",
			"QQ_TOKEN_URL":    "https://unsupported.invalid/token",
		})
		process.waitForReady(t)
		process.terminate(t)
		if err := process.wait(); err != nil {
			t.Fatalf("fixed-endpoint service exit: %v", err)
		}
		process.requireEvents(t, "qq_token_refreshed", "service_started", "service_stopped")
	})
	t.Run("occupied listener exits once", func(t *testing.T) {
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		process := launchServiceProcess(t, binary, map[string]string{"LISTEN_ADDR": listener.Addr().String()})
		process.requireStartupFailure(t)
	})
	t.Run("terminal gateway failure exits once", func(t *testing.T) {
		upstream := newProcessQQServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/gateway/bot" {
				_ = json.NewEncoder(w).Encode(map[string]string{"url": "wss://" + r.Host + "/socket"})
				return
			}
			if r.URL.Path != "/socket" {
				_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
				return
			}
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = connection.CloseNow() }()
			if wsjson.Write(t.Context(), connection, map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 100}}) != nil {
				return
			}
			var authentication map[string]any
			if wsjson.Read(t.Context(), connection, &authentication) != nil {
				return
			}
			_ = connection.Close(4014, "test-secret private close reason")
		}))
		defer upstream.Close()
		certificatePath := filepath.Join(t.TempDir(), "gateway.pem")
		certificate := processQQCertificate(upstream)
		if err := os.WriteFile(certificatePath, certificate, 0600); err != nil {
			t.Fatal(err)
		}
		process := launchServiceProcess(t, binary, map[string]string{"SSL_CERT_FILE": certificatePath}, upstream)
		var exitError *exec.ExitError
		if err := process.wait(); !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			t.Fatalf("gateway failure exit = %v, want status 1", err)
		}
		failures := 0
		for _, event := range process.events(t) {
			if event["level"] == "ERROR" {
				if event["event"] != "qq_gateway_failed" {
					t.Fatalf("gateway error was logged at the wrong boundary: %v", event)
				}
				failures++
			}
		}
		if failures != 1 {
			t.Fatalf("gateway error was logged %d times, want once", failures)
		}
		logs, err := os.ReadFile(process.logPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(logs), "private close reason") || strings.Contains(string(logs), "test-secret") {
			t.Fatal("gateway failure leaked its close reason")
		}
	})
	for _, prefix := range []string{"api/v1", "//api", "/api//v1", "/api/../v1", "/api/./v1", "/api/%2Fv1", "/api?x=y", "/api#fragment", "/api/:id", "/api/*", "/api/with space", "/api//"} {
		t.Run("invalid API prefix "+prefix, func(t *testing.T) {
			process := launchServiceProcess(t, binary, map[string]string{"API_PATH_PREFIX": prefix})
			process.requireStartupFailure(t)
			failure := process.events(t)[0]["error"].(map[string]any)
			if failure["context"].(map[string]any)["setting"] != "API_PATH_PREFIX" {
				t.Fatalf("invalid prefix lost configuration diagnostics: %v", failure)
			}
		})
	}
	for _, scenario := range []struct{ prefix, normalized string }{
		{"", ""}, {"/", ""}, {"/api/v1", "/api/v1"}, {" /api/notify-qq/ ", "/api/notify-qq"},
	} {
		t.Run("API prefix "+scenario.prefix, func(t *testing.T) {
			process := launchServiceProcess(t, binary, map[string]string{"API_PATH_PREFIX": scenario.prefix})
			address := process.waitForReady(t)
			if !strings.HasSuffix(address, scenario.normalized) {
				t.Fatalf("prefix was not normalized: %v", process.events(t))
			}
			process.terminate(t)
			if err := process.wait(); err != nil {
				t.Fatalf("prefixed service exit: %v", err)
			}
			process.requireEvents(t, "service_started", "service_stopped")
		})
	}
	t.Run("SIGTERM exits successfully", func(t *testing.T) {
		process := launchServiceProcess(t, binary, nil)
		process.waitForReady(t)
		process.terminate(t)
		if err := process.wait(); err != nil {
			t.Fatalf("graceful exit: %v", err)
		}
		process.requireEvents(t, "service_started", "service_stopped")
	})
	t.Run("SIGTERM drains an accepted request", func(t *testing.T) {
		messageStarted := make(chan struct{})
		releaseMessage := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseMessage) })
		upstream := newProcessQQServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if serveProcessGatewayRoute(t, w, r) {
				return
			}
			if r.URL.Path == "/app/getAppAccessToken" {
				_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
				return
			}
			close(messageStarted)
			select {
			case <-releaseMessage:
				_, _ = io.WriteString(w, `{"id":"drained-message"}`)
			case <-r.Context().Done():
			}
		}))
		defer upstream.Close()
		defer release()
		certificate := processQQCertificate(upstream)
		certificatePath := filepath.Join(t.TempDir(), "upstream.pem")
		if err := os.WriteFile(certificatePath, certificate, 0600); err != nil {
			t.Fatal(err)
		}
		process := launchServiceProcess(t, binary, map[string]string{
			"SSL_CERT_FILE": certificatePath,
		}, upstream)
		address := process.waitForReady(t)
		requestCtx, cancelRequest := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelRequest()
		request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, address+"/notify", strings.NewReader(`{"content":"finish before shutdown"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+process.apiKey)
		request.Header.Set("Content-Type", "application/json")
		type notificationResult struct {
			status int
			body   string
			err    error
		}
		// One result slot lets the bounded request finish even if an assertion
		// fails; cleanup cancels the request and releases the mock upstream.
		result := make(chan notificationResult, 1)
		go func() {
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				result <- notificationResult{err: err}
				return
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			result <- notificationResult{status: response.StatusCode, body: string(body), err: err}
		}()
		select {
		case <-messageStarted:
		case <-requestCtx.Done():
			t.Fatal("notification did not reach upstream")
		}
		process.terminate(t)
		waitForProcessCondition(t, func() bool {
			connection, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(t.Context(), "tcp", strings.TrimPrefix(address, "http://"))
			if err != nil {
				return true
			}
			_ = connection.Close()
			return false
		})
		release()
		select {
		case notification := <-result:
			if notification.err != nil || notification.status != http.StatusOK || !strings.Contains(notification.body, "drained-message") {
				t.Fatalf("in-flight notification did not finish: %+v", notification)
			}
		case <-requestCtx.Done():
			t.Fatal("notification did not finish during shutdown")
		}
		if err := process.wait(); err != nil {
			t.Fatalf("graceful exit: %v", err)
		}
		process.requireEvents(t, "notification_accepted", "service_stopped")
	})
}

type serviceProcess struct {
	command *exec.Cmd
	logPath string
	waited  bool
	apiKey  string
}

func launchServiceProcess(t *testing.T, binary string, overrides map[string]string, upstreams ...*httptest.Server) *serviceProcess {
	t.Helper()
	directory := t.TempDir()
	// File configuration verifies environment precedence, including clearing a
	// previously configured prefix with an explicit empty environment value.
	fileSettings := "API_PATH_PREFIX=/from-file\nQQ_API_BASE_URL=https://unsupported.invalid\nQQ_TOKEN_URL=https://unsupported.invalid/token\nOTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:1\nOTEL_SERVICE_NAME=from-file\nOTEL_TRACES_SAMPLER_ARG=0\n"
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte(fileSettings), 0600); err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{
		"QQ_APP_ID": "test-app", "QQ_APP_SECRET": "test-secret",
		"LISTEN_ADDR": "127.0.0.1:0", "DATA_DIR": directory,
		"API_PATH_PREFIX":             "",
		"LOG_LEVEL":                   "info",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "",
	}
	var upstream *httptest.Server
	if len(upstreams) > 0 {
		upstream = upstreams[0]
	} else {
		upstream = newProcessQQServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if serveProcessGatewayRoute(t, w, r) {
				return
			}
			if r.URL.Path != "/app/getAppAccessToken" || r.Host != "api.bot.qq.com" {
				t.Errorf("unexpected QQ endpoint: %s %s", r.Host, r.URL.Path)
				http.Error(w, "unexpected endpoint", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
		}))
		t.Cleanup(upstream.Close)
		certificate := processQQCertificate(upstream)
		certificatePath := filepath.Join(directory, "gateway.pem")
		if err := os.WriteFile(certificatePath, certificate, 0600); err != nil {
			t.Fatal(err)
		}
		settings["SSL_CERT_FILE"] = certificatePath
	}
	proxy := newProcessQQProxy(t, upstream)
	settings["HTTPS_PROXY"] = proxy.URL
	settings["HTTP_PROXY"] = proxy.URL
	settings["NO_PROXY"] = "127.0.0.1,localhost,::1"
	maps.Copy(settings, overrides)
	created := runKeyCommand(t, binary, settings["DATA_DIR"], []string{"keys", "create", "--name", "process-fixture"}, 0)
	var credential struct {
		Key         string `json:"key"`
		PairingCode string `json:"pairing_code"`
	}
	if err := json.Unmarshal(created, &credential); err != nil {
		t.Fatal(err)
	}
	keys, err := apikey.Open(t.Context(), settings["DATA_DIR"])
	if err != nil {
		t.Fatal(err)
	}
	_, _, bindError := keys.Bind(t.Context(), credential.PairingCode, "owner-openid")
	if err := errors.Join(bindError, keys.Close()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, binary)
	command.Dir = directory
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := settings[key]; !overridden && !strings.HasPrefix(key, "OTEL_") {
			command.Env = append(command.Env, entry)
		}
	}
	for key, value := range settings {
		command.Env = append(command.Env, key+"="+value)
	}
	logPath := filepath.Join(directory, "stderr.jsonl")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = logFile
	err = command.Start()
	if closeError := logFile.Close(); closeError != nil {
		t.Error(closeError)
	}
	if err != nil {
		t.Fatal(err)
	}
	process := &serviceProcess{command: command, logPath: logPath, apiKey: credential.Key}
	t.Cleanup(func() {
		if !process.waited {
			_ = command.Process.Kill()
			_ = process.wait()
		}
	})
	return process
}

func serveProcessGatewayRoute(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.URL.Path == "/gateway/bot" {
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "wss://" + r.Host + "/gateway/socket"})
		return true
	}
	if r.URL.Path != "/gateway/socket" {
		return false
	}
	connection, err := websocket.Accept(w, r, nil)
	if err != nil {
		t.Error(err)
		return true
	}
	defer func() { _ = connection.CloseNow() }()
	ctx := t.Context()
	if wsjson.Write(ctx, connection, map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 100}}) != nil {
		return true
	}
	var authentication map[string]any
	if wsjson.Read(ctx, connection, &authentication) != nil {
		return true
	}
	if wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]string{"session_id": "process-session"}}) != nil {
		return true
	}
	for {
		var heartbeat map[string]any
		if wsjson.Read(ctx, connection, &heartbeat) != nil || wsjson.Write(ctx, connection, map[string]int{"op": 11}) != nil {
			return true
		}
	}
}

func (p *serviceProcess) wait() error {
	p.waited = true
	return p.command.Wait()
}

func (p *serviceProcess) terminate(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

func (p *serviceProcess) events(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for line := range strings.SplitSeq(string(data), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

func (p *serviceProcess) waitForReady(t *testing.T) string {
	t.Helper()
	var address string
	waitForProcessCondition(t, func() bool {
		for _, event := range p.events(t) {
			if event["event"] == "service_started" {
				address = "http://" + event["listen_addr"].(string) + event["api_path_prefix"].(string)
			}
		}
		if address == "" {
			return false
		}
		client := &http.Client{Timeout: 100 * time.Millisecond}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address+"/readyz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return false
		}
		statusRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address+"/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		statusRequest.Header.Set("Authorization", "Bearer "+p.apiKey)
		statusResponse, err := client.Do(statusRequest)
		if err != nil {
			return false
		}
		defer func() { _ = statusResponse.Body.Close() }()
		var state struct {
			WebSocketConnected bool `json:"websocket_connected"`
		}
		if json.NewDecoder(statusResponse.Body).Decode(&state) != nil {
			return false
		}
		return statusResponse.StatusCode == http.StatusOK && state.WebSocketConnected
	})
	return address
}

func (p *serviceProcess) requireStartupFailure(t *testing.T) {
	t.Helper()
	var exitError *exec.ExitError
	if err := p.wait(); !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("startup exit = %v, want status 1", err)
	}
	events := p.events(t)
	failures := 0
	for _, event := range events {
		if event["event"] == "service_started" {
			t.Fatal("failed startup reported a running service")
		}
		if event["level"] == "ERROR" {
			failures++
			if event["event"] != "startup_failed" || event["error"] == nil {
				t.Fatalf("startup error at the wrong boundary: %v", event)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("startup failure must be logged exactly once: %v", events)
	}
}

func (p *serviceProcess) requireEvents(t *testing.T, expected ...string) {
	t.Helper()
	events := p.events(t)
	for _, name := range expected {
		count := 0
		for _, event := range events {
			if event["level"] == "ERROR" {
				t.Fatalf("successful lifecycle logged an error: %v", event)
			}
			if event["event"] == name {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("event %q occurred %d times, want once: %v", name, count, events)
		}
	}
}

func waitForProcessCondition(t *testing.T, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("process condition did not become ready within five seconds")
		}
	}
}
