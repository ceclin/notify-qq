package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ceclin/notify-qq/internal/logging"
)

func TestBehaviorAPIPathPrefixScopesAllEndpoints(t *testing.T) {
	const prefix = "/api/notify-qq"
	fixture := startBehaviorServiceWithPrefix(t, "", nil, prefix)
	status, _, _ := fixture.request(t, http.MethodGet, prefix+"/healthz", "", nil)
	requireStatus(t, status, http.StatusOK)
	status, _, _ = fixture.request(t, http.MethodGet, prefix+"/status", "", nil)
	requireStatus(t, status, http.StatusUnauthorized)
	authorization := map[string]string{"Authorization": "Bearer " + fixture.apiKey}
	status, result, _ := fixture.request(t, http.MethodGet, prefix+"/status", "", authorization)
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != false {
		t.Fatal("unexpected initial recipient")
	}
	status, _, _ = fixture.notify(t, `{"content":"before pairing"}`)
	requireStatus(t, status, http.StatusConflict)
	if err := fixture.deliverMessage(t, "owner-openid", "bind "+fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	status, result, _ = fixture.request(t, http.MethodGet, prefix+"/status", "", authorization)
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != true || result["user_openid"] != "owner-openid" {
		t.Fatal("WebSocket event did not pair the recipient")
	}
	status, result, _ = fixture.notify(t, `{"content":"under the prefix"}`)
	requireStatus(t, status, http.StatusOK)
	if result["message_id"] != "qq-message-id" {
		t.Fatal("prefixed notification was not accepted")
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/healthz"}, {http.MethodGet, "/status"},
		{http.MethodPost, "/notify"}, {http.MethodPost, "/qq/webhook"},
		{http.MethodPost, prefix + "/qq/webhook"}, {http.MethodGet, prefix + "-extra/healthz"}, {http.MethodGet, prefix + "/extra/healthz"},
	} {
		status, _, _ := fixture.request(t, route.method, route.path, "", nil)
		requireStatus(t, status, http.StatusNotFound)
	}
	status, _, headers := fixture.request(t, http.MethodGet, prefix+"/notify", "", nil)
	requireStatus(t, status, http.StatusMethodNotAllowed)
	if !slices.Contains(strings.Split(headers.Get("Allow"), ", "), http.MethodPost) {
		t.Fatalf("incorrect prefixed method rejection: %v", headers)
	}
	if fixture.sendCalls.Load() != 1 || fixture.tokenCalls.Load() != 1 {
		t.Fatal("invalid prefixed requests contacted QQ")
	}
}

func TestBehaviorPrefixedLogsKeepCorrelationAndQuietProbes(t *testing.T) {
	const prefix = "/api/v1"
	var captured bytes.Buffer
	logger := logging.NewLogger(&captured, slog.LevelInfo)
	fixture := startBehaviorServiceWithPrefixAndLogger(t, "owner-openid", nil, prefix, logger)
	captured.Reset()
	status, _, _ := fixture.request(t, http.MethodGet, prefix+"/healthz", "", nil)
	requireStatus(t, status, http.StatusOK)
	status, _, _ = fixture.request(t, http.MethodGet, prefix+"/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	status, _, headers := fixture.notify(t, `{"content":"prefixed workflow"}`)
	requireStatus(t, status, http.StatusOK)
	fixture.server.Close()
	decoder := json.NewDecoder(bytes.NewReader(captured.Bytes()))
	accepted := 0
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["route"] != prefix+"/notify" || entry["request_id"] != headers.Get("X-Request-ID") {
			t.Fatalf("prefixed correlation was lost or a probe emitted INFO noise: %v", entry)
		}
		if entry["event"] == "notification_accepted" {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted events = %d, want once", accepted)
	}
}
