package app

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ceclin/notify-qq/internal/apikey"
	"github.com/ceclin/notify-qq/internal/logging"
)

func TestBehaviorReadinessIsPublicPrefixedAndIndependentOfQQ(t *testing.T) {
	fixture := startBehaviorServiceWithPrefix(t, "", nil, "/api")
	database, err := sql.Open("sqlite", filepath.Join(fixture.settings.DataDir, apikey.Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := database.ExecContext(t.Context(), "DELETE FROM api_keys"); err != nil {
		t.Fatal(err)
	}
	status, result, headers := fixture.request(t, http.MethodGet, "/api/readyz", "", nil)
	requireStatus(t, status, http.StatusOK)
	if result["status"] != "ready" || headers.Get("X-Request-ID") == "" || headers.Get("Cache-Control") != "no-store" {
		t.Fatal("readiness response lost contract/security headers")
	}
	if fixture.service.qq.GatewayConnected() || fixture.tokenCalls.Load() != 0 || fixture.sendCalls.Load() != 0 {
		t.Fatal("readiness contacted QQ or depended on a connection")
	}
	status, _, _ = fixture.request(t, http.MethodGet, "/readyz", "", nil)
	requireStatus(t, status, http.StatusNotFound)
	status, _, headers = fixture.request(t, http.MethodPost, "/api/readyz", `{}`, nil)
	requireStatus(t, status, http.StatusMethodNotAllowed)
	if !strings.Contains(headers.Get("Allow"), "GET") {
		t.Fatal("readiness routing lost Allow header")
	}
}

func TestBehaviorReadinessReportsStorageTransitionsWithoutProbeNoise(t *testing.T) {
	var logs lockedLogBuffer
	logger := logging.NewLogger(&logs, slog.LevelInfo)
	fixture := startBehaviorServiceWithPrefixAndLogger(t, "owner-openid", nil, "/api", logger)
	status, _, _ := fixture.request(t, http.MethodGet, "/api/readyz", "", nil)
	requireStatus(t, status, http.StatusOK)
	if logs.String() != "" {
		t.Fatal("healthy readiness emitted INFO noise")
	}
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
	for range 3 {
		status, result, headers := fixture.request(t, http.MethodGet, "/api/readyz", "", nil)
		requireStatus(t, status, http.StatusServiceUnavailable)
		if result["error"].(map[string]any)["message"] != "Service is not ready" || result["request_id"] != headers.Get("X-Request-ID") {
			t.Fatal("readiness failure leaked diagnostics or lost correlation")
		}
	}
	status, _, _ = fixture.request(t, http.MethodGet, "/api/healthz", "", nil)
	requireStatus(t, status, http.StatusOK)
	if _, err := database.ExecContext(t.Context(), "ALTER TABLE unavailable_keys RENAME TO api_keys"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		status, _, _ := fixture.request(t, http.MethodGet, "/api/readyz", "", nil)
		requireStatus(t, status, http.StatusOK)
	}
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	failed, recovered := 0, 0
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["event"] != "readiness_changed" {
			t.Fatalf("probe emitted unrelated INFO/WARN logs: %v", event)
		}
		if event["ready"] == false {
			failed++
			if event["level"] != "WARN" || event["error"] == nil || event["request_id"] == nil {
				t.Fatal("storage failure missing owning boundary/error context")
			}
		} else {
			recovered++
			if event["level"] != "INFO" {
				t.Fatal("readiness recovery used incorrect level")
			}
		}
	}
	if failed != 1 || recovered != 1 {
		t.Fatal("readiness transitions repeated or were lost")
	}
}

func TestBehaviorReadinessTracksLifecycleAndClosedStorage(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", nil)
	fixture.service.running.Store(false)
	status, _, _ := fixture.request(t, http.MethodGet, "/readyz", "", nil)
	requireStatus(t, status, http.StatusServiceUnavailable)
	status, _, _ = fixture.request(t, http.MethodGet, "/healthz", "", nil)
	requireStatus(t, status, http.StatusOK)
	fixture.service.running.Store(true)
	status, _, _ = fixture.request(t, http.MethodGet, "/readyz", "", nil)
	requireStatus(t, status, http.StatusOK)
	if err := fixture.service.keys.Close(); err != nil {
		t.Fatal(err)
	}
	status, _, _ = fixture.request(t, http.MethodGet, "/readyz", "", nil)
	requireStatus(t, status, http.StatusServiceUnavailable)
	status, _, _ = fixture.request(t, http.MethodGet, "/healthz", "", nil)
	requireStatus(t, status, http.StatusOK)
}
