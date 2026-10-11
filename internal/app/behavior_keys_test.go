package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ceclin/notify-qq/internal/logging"

	"github.com/ceclin/notify-qq/internal/apikey"
)

func TestBehaviorStoredAPIKeysAndRevocationAcrossRestart(t *testing.T) {
	fixture := startBehaviorServiceWithPrefix(t, "owner-openid", nil, "/api")
	management, err := apikey.Open(t.Context(), fixture.settings.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := management.Close(); err != nil {
			t.Error(err)
		}
	}()
	firstCreated, err := management.Create(t.Context(), "backup")
	first, firstSecret, firstCode := firstCreated.Key, firstCreated.APIKey, firstCreated.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	secondCreated, err := management.Create(t.Context(), "deployment")
	secondSecret, secondCode := secondCreated.APIKey, secondCreated.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := management.Bind(t.Context(), firstCode, "owner-openid"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := management.Bind(t.Context(), secondCode, "owner-openid"); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{firstSecret, secondSecret, fixture.apiKey} {
		status, _, _ := fixture.request(t, http.MethodPost, "/api/notify", `{"content":"multi key"}`, map[string]string{"Authorization": "Bearer " + secret})
		requireStatus(t, status, http.StatusOK)
	}
	if changed, err := management.Revoke(t.Context(), first.ID); err != nil || !changed {
		t.Fatalf("revoke: changed=%v, err=%v", changed, err)
	}
	if changed, err := management.Revoke(t.Context(), first.ID); err != nil || changed {
		t.Fatalf("repeat revoke: changed=%v, err=%v", changed, err)
	}
	for _, path := range []string{"/api/status", "/api/notify"} {
		method, body := http.MethodGet, ""
		if strings.HasSuffix(path, "notify") {
			method, body = http.MethodPost, `{"content":"revoked"}`
		}
		status, _, _ := fixture.request(t, method, path, body, map[string]string{"Authorization": "Bearer " + firstSecret})
		requireStatus(t, status, http.StatusUnauthorized)
	}
	initial, valid, err := management.Authenticate(t.Context(), fixture.apiKey)
	if err != nil || !valid {
		t.Fatalf("fixture key missing: %v", err)
	}
	if _, err := management.Revoke(t.Context(), initial.ID); err != nil {
		t.Fatal(err)
	}
	fixture.server.Close()
	if err := fixture.service.keys.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := fixture.assemble(t)
	fixture.service = restarted
	fixture.server = httptest.NewServer(restarted.routes())
	for _, secret := range []string{firstSecret, fixture.apiKey} {
		status, _, _ := fixture.request(t, http.MethodPost, "/api/notify", `{"content":"revoked after restart"}`, map[string]string{"Authorization": "Bearer " + secret})
		requireStatus(t, status, http.StatusUnauthorized)
	}
	status, _, _ := fixture.request(t, http.MethodPost, "/api/notify", `{"content":"still active"}`, map[string]string{"Authorization": "Bearer " + secondSecret})
	requireStatus(t, status, http.StatusOK)
	if fixture.sendCalls.Load() != 4 {
		t.Fatalf("rejected keys contacted QQ: sends=%d", fixture.sendCalls.Load())
	}
	for _, path := range []string{"/keys", "/api/keys"} {
		status, _, _ := fixture.request(t, http.MethodPost, path, `{}`, map[string]string{"Authorization": "Bearer " + secondSecret})
		requireStatus(t, status, http.StatusNotFound)
	}
}

func TestBehaviorKeyDatabaseFailureFailsClosed(t *testing.T) {
	var captured bytes.Buffer
	logger := logging.NewLogger(&captured, slog.LevelDebug)
	fixture := startBehaviorServiceWithLogger(t, "owner-openid", nil, logger)
	if err := fixture.service.keys.Close(); err != nil {
		t.Fatal(err)
	}
	captured.Reset()
	status, result, _ := fixture.notify(t, `{"content":"must never reach QQ"}`)
	requireStatus(t, status, http.StatusInternalServerError)
	if result["error"].(map[string]any)["message"] != "Authentication could not be checked" {
		t.Fatalf("unsafe public error: %v", result)
	}
	if fixture.sendCalls.Load() != 0 || fixture.tokenCalls.Load() != 0 {
		t.Fatal("failed authentication contacted QQ")
	}
	if strings.Count(captured.String(), `"event":"http_request_failed"`) != 1 || strings.Count(captured.String(), `"level":"ERROR"`) != 1 {
		t.Fatalf("database failure not logged once: %s", captured.String())
	}
	if strings.Contains(captured.String(), fixture.apiKey) {
		t.Fatal("key leaked in database failure log")
	}
}

func TestBehaviorStoredKeyCorrelation(t *testing.T) {
	var captured bytes.Buffer
	logger := logging.NewLogger(&captured, slog.LevelDebug)
	fixture := startBehaviorServiceWithLogger(t, "owner-openid", nil, logger)
	created, err := fixture.service.keys.Create(t.Context(), "backup")
	key, secret, code := created.Key, created.APIKey, created.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.service.keys.Bind(t.Context(), code, "owner-openid"); err != nil {
		t.Fatal(err)
	}
	captured.Reset()
	status, _, headers := fixture.request(t, http.MethodPost, "/notify", `{"content":"private report"}`, map[string]string{"Authorization": "Bearer " + secret})
	requireStatus(t, status, http.StatusOK)
	matched := 0
	for entry := range strings.SplitSeq(strings.TrimSpace(captured.String()), "\n") {
		if strings.Contains(entry, `"event":"notification_`) || strings.Contains(entry, `"event":"qq_`) || strings.Contains(entry, `"event":"http_request_completed"`) {
			matched++
			if !strings.Contains(entry, `"api_key_id":"`+key.ID+`"`) || !strings.Contains(entry, `"request_id":"`+headers.Get("X-Request-ID")+`"`) {
				t.Fatalf("lost key/request correlation: %s", entry)
			}
		}
	}
	if matched < 3 {
		t.Fatalf("missing structured workflow logs: %s", captured.String())
	}
	if strings.Contains(captured.String(), secret) || strings.Contains(captured.String(), "private report") {
		t.Fatal("sensitive data appeared in logs")
	}
}
