package app

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ceclin/notify-qq/internal/logging"
)

func TestBehaviorPairingReplyFailureKeepsBinding(t *testing.T) {
	var reject atomic.Bool
	reject.Store(true)
	var logs lockedLogBuffer
	fixture := startBehaviorServiceWithLogger(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
		} else if reject.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"code":400123,"message":"private-upstream-error"}`)
		} else {
			_, _ = io.WriteString(w, `{"id":"qq-message-id"}`)
		}
	}, logging.NewLogger(&logs, slog.LevelDebug))
	if err := fixture.deliverMessage(t, "owner-openid", "bind "+fixture.pairingCode); err != nil {
		t.Fatalf("confirmation failure must not fail the persisted binding: %v", err)
	}
	status, result, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != true || result["user_openid"] != "owner-openid" {
		t.Fatal("confirmation failure lost the recipient")
	}
	if err := fixture.deliverMessage(t, "intruder", "bind "+fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	if fixture.sendCalls.Load() != 1 {
		t.Fatal("a consumed code retried the confirmation")
	}
	reject.Store(false)
	status, _, _ = fixture.notify(t, `{"content":"after confirmation failure"}`)
	requireStatus(t, status, http.StatusOK)
	if fixture.messages[1]["recipient"] != "owner-openid" || fixture.messages[1]["msg_id"] != nil {
		t.Fatal("confirmation failure changed subsequent notification delivery")
	}
	entries := logs.String()
	if strings.Count(entries, `"event":"pairing_reply_failed"`) != 1 || strings.Contains(entries, `"event":"pairing_reply_accepted"`) {
		t.Fatal("confirmation failure was not reported once at its owning boundary")
	}
	for _, private := range []string{fixture.apiKey, fixture.pairingCode, "private-upstream-error"} {
		if strings.Contains(entries, private) {
			t.Fatal("confirmation error exposed private data")
		}
	}
}

func TestBehaviorPairingWithoutMessageIDDoesNotConsumeCode(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	if err := fixture.deliverMessageWithID(t, "owner-openid", "bind "+fixture.pairingCode, ""); err == nil {
		t.Fatal("binding without a reply message ID was accepted")
	}
	status, result, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != false || fixture.sendCalls.Load() != 0 {
		t.Fatal("missing message ID changed binding state or sent a proactive reply")
	}
	if err := fixture.deliverMessage(t, "owner-openid", "bind "+fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	status, result, _ = fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != true || fixture.sendCalls.Load() != 1 {
		t.Fatal("invalid event consumed the pairing code")
	}
}
