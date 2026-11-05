package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ceclin/notify-qq/internal/logging"
)

func TestBehaviorGroupPairingAndNotificationsSurviveRestartAndRebinding(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	if err := fixture.deliverGroupMessage(t, "target-group", "member", " \n"+fixture.pairingCode+"\n "); err != nil {
		t.Fatal(err)
	}
	status, state, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	requireStatusTarget(t, state, "group", "target-group")
	if _, oldField := state["user_openid"]; oldField {
		t.Fatal("status still exposes an old user-only field")
	}
	reply := fixture.messages[0]
	if reply["recipient_type"] != "group" || reply["recipient"] != "target-group" || reply["msg_id"] != "binding-message" || reply["content"] != "Paired successfully. Notifications are ready." {
		t.Fatal("group binding confirmation was not sent back to the group")
	}
	status, _, _ = fixture.notify(t, `{"content":"## Group report","buttons":[{"label":"Details","url":"https://example.com/logs"}]}`)
	requireStatus(t, status, http.StatusOK)
	notification := fixture.messages[1]
	if notification["recipient_type"] != "group" || notification["recipient"] != "target-group" || notification["msg_type"] != float64(2) || notification["msg_id"] != nil || notification["keyboard"] == nil {
		t.Fatal("notification did not use the group's proactive Markdown endpoint")
	}
	fixture.server.Close()
	if err := fixture.service.keys.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.service = fixture.assemble(t)
	fixture.server = httptest.NewServer(fixture.service.routes())
	status, state, _ = fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	requireStatusTarget(t, state, "group", "target-group")
	if err := fixture.deliverGroupMessage(t, "other-group", "intruder", fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	if fixture.messages[2]["recipient"] != "other-group" || fixture.messages[2]["content"] != "Pairing code is invalid or expired. Please generate a new one." {
		t.Fatal("consumed group code was accepted again")
	}
	key, valid, err := fixture.service.keys.Authenticate(t.Context(), fixture.apiKey)
	if err != nil || !valid {
		t.Fatal("bound API key could not be read")
	}
	issued, err := fixture.service.keys.IssuePairing(t.Context(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ = fixture.notify(t, `{"content":"before replacement"}`)
	requireStatus(t, status, http.StatusOK)
	if fixture.messages[3]["recipient_type"] != "group" || fixture.messages[3]["recipient"] != "target-group" {
		t.Fatal("issuing a code changed the active recipient")
	}
	if err := fixture.deliverMessage(t, "new-owner", issued.PairingCode); err != nil {
		t.Fatal(err)
	}
	status, state, _ = fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	requireStatusTarget(t, state, "user", "new-owner")
	status, _, _ = fixture.notify(t, `{"content":"after replacement"}`)
	requireStatus(t, status, http.StatusOK)
	if fixture.messages[5]["recipient_type"] != "user" || fixture.messages[5]["recipient"] != "new-owner" {
		t.Fatal("replacing a group with a user did not change the send endpoint")
	}
	issued, err = fixture.service.keys.IssuePairing(t.Context(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverGroupMessage(t, "replacement-group", "another-member", issued.PairingCode); err != nil {
		t.Fatal(err)
	}
	status, _, _ = fixture.notify(t, `{"content":"back to a group"}`)
	requireStatus(t, status, http.StatusOK)
	if fixture.messages[7]["recipient_type"] != "group" || fixture.messages[7]["recipient"] != "replacement-group" {
		t.Fatal("replacing a user with a group did not change the send endpoint")
	}
}

func TestBehaviorUsersAndGroupsWithTheSameOpenIDStayIsolated(t *testing.T) {
	var logs lockedLogBuffer
	fixture := startBehaviorServiceWithLogger(t, "same-openid", nil, logging.NewLogger(&logs, slog.LevelDebug))
	group, err := fixture.service.keys.Create(t.Context(), "group-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverGroupMessage(t, "same-openid", "member", group.PairingCode); err != nil {
		t.Fatal(err)
	}
	status, _, _ := fixture.notify(t, `{"content":"user report"}`)
	requireStatus(t, status, http.StatusOK)
	status, _, _ = fixture.request(t, http.MethodPost, "/notify", `{"content":"group report"}`, map[string]string{"Authorization": "Bearer " + group.APIKey})
	requireStatus(t, status, http.StatusOK)
	if fixture.messages[1]["recipient_type"] != "user" || fixture.messages[2]["recipient_type"] != "group" || fixture.messages[1]["recipient"] != fixture.messages[2]["recipient"] {
		t.Fatal("recipient type did not isolate identical user and group IDs")
	}
	status, _, _ = fixture.request(t, http.MethodPost, "/notify", `{"content":"override","recipient":{"type":"user","openid":"other"}}`, map[string]string{"Authorization": "Bearer " + group.APIKey})
	requireStatus(t, status, http.StatusBadRequest)
	if fixture.sendCalls.Load() != 3 {
		t.Fatal("request body overrode the key's recipient")
	}
	refs := map[string]string{}
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["event"] == "notification_started" {
			refs[entry["recipient_type"].(string)] = entry["recipient_ref"].(string)
		}
	}
	if refs["user"] == "" || refs["group"] == "" || refs["user"] == refs["group"] || strings.Contains(logs.String(), "same-openid") {
		t.Fatal("recipient logs lost type isolation or exposed an OpenID")
	}
}

func TestBehaviorGroupMessagesRequireMentionsAndCompleteMetadata(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	if err := fixture.deliverQQEvent(t, "GROUP_MESSAGE_CREATE", map[string]any{"id": "group-message", "group_openid": "target-group", "content": fixture.pairingCode, "author": map[string]string{"member_openid": "member"}}); err != nil {
		t.Fatal(err)
	}
	for _, data := range []map[string]any{
		{"id": "group-message", "content": fixture.pairingCode, "author": map[string]string{"member_openid": "member", "user_openid": "no-fallback"}},
		{"id": "group-message", "group_openid": "target-group", "content": fixture.pairingCode, "author": map[string]string{"user_openid": "no-fallback"}},
		{"group_openid": "target-group", "content": fixture.pairingCode, "author": map[string]string{"member_openid": "member"}},
	} {
		if err := fixture.deliverQQEvent(t, "GROUP_AT_MESSAGE_CREATE", data); err == nil {
			t.Fatal("incomplete group event was accepted")
		}
	}
	if fixture.sendCalls.Load() != 0 {
		t.Fatal("unmentioned or invalid group event produced a message")
	}
	if err := fixture.deliverGroupMessage(t, "target-group", "member", "hello"); err != nil {
		t.Fatal(err)
	}
	reply := fixture.messages[0]
	allowed := map[string]bool{"Got it.": true, "Noted.": true, "Copy that.": true, "Received.": true}
	if reply["recipient_type"] != "group" || reply["recipient"] != "target-group" || !allowed[reply["content"].(string)] || reply["msg_id"] != "binding-message" {
		t.Fatal("group mention was not acknowledged in the group")
	}
	status, state, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	requireStatusTarget(t, state, "", "")
}

func TestBehaviorGroupReplyFailureKeepsBindingAndDoesNotExposeGroupID(t *testing.T) {
	var reject atomic.Bool
	reject.Store(true)
	var logs lockedLogBuffer
	fixture := startBehaviorServiceWithLogger(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
		} else if reject.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":40034101,"message":"private-group-openid"}`)
		} else {
			_, _ = io.WriteString(w, `{"id":"qq-message-id"}`)
		}
	}, logging.NewLogger(&logs, slog.LevelDebug))
	if err := fixture.deliverGroupMessage(t, "private-group-openid", "member", fixture.pairingCode); err != nil {
		t.Fatal(err)
	}
	status, state, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	requireStatusTarget(t, state, "group", "private-group-openid")
	status, _, _ = fixture.notify(t, `{"content":"rejected group report"}`)
	requireStatus(t, status, http.StatusBadGateway)
	if strings.Contains(logs.String(), "private-group-openid") || strings.Contains(logs.String(), fixture.pairingCode) {
		t.Fatal("group reply or notification failure exposed private data")
	}
	reject.Store(false)
	status, _, _ = fixture.notify(t, `{"content":"group report after recovery"}`)
	requireStatus(t, status, http.StatusOK)
	if fixture.messages[2]["recipient_type"] != "group" {
		t.Fatal("group failure changed the persisted recipient type")
	}
}
