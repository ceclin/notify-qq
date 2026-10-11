package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ceclin/notify-qq/internal/logging"

	"github.com/ceclin/notify-qq/internal/apikey"
)

func TestBehaviorKeyRecipientsStayIsolatedAcrossRebindingAndRestart(t *testing.T) {
	var logs lockedLogBuffer
	logger := logging.NewLogger(&logs, slog.LevelDebug)
	fixture := startBehaviorServiceWithPrefixAndLogger(t, "", nil, "/api", logger)
	firstCreated, err := fixture.service.keys.Create(t.Context(), "alice-backup")
	first, firstSecret, firstCode := firstCreated.Key, firstCreated.APIKey, firstCreated.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	secondCreated, err := fixture.service.keys.Create(t.Context(), "bob-deploy")
	second, secondSecret, secondCode := secondCreated.Key, secondCreated.APIKey, secondCreated.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	request := func(secret, path, body string, expected int) map[string]any {
		method := http.MethodGet
		if body != "" {
			method = http.MethodPost
		}
		status, result, _ := fixture.request(t, method, "/api"+path, body, map[string]string{"Authorization": "Bearer " + secret})
		requireStatus(t, status, expected)
		return result
	}
	requireRecipient := func(secret, user string) {
		result := request(secret, "/status", "", http.StatusOK)
		if result["user_openid"] != user || result["paired"] != (user != "") {
			t.Fatalf("key-scoped status mismatch: %v", result)
		}
	}
	requireRecipient(firstSecret, "")
	requireRecipient(secondSecret, "")
	request(firstSecret, "/notify", `{"content":"unpaired"}`, http.StatusConflict)
	request(firstCode, "/status", "", http.StatusUnauthorized)
	if fixture.tokenCalls.Load() != 0 {
		t.Fatal("unpaired notification contacted QQ")
	}
	if err := fixture.deliverMessage(t, "alice", "bind "+firstCode); err != nil {
		t.Fatal(err)
	}
	requireRecipient(firstSecret, "alice")
	requireRecipient(secondSecret, "")
	if err := fixture.deliverMessage(t, "bob", "bind "+secondCode); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "intruder", "bind "+firstCode); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "intruder", "bind "+firstSecret); err != nil {
		t.Fatal(err)
	}
	requireRecipient(firstSecret, "alice")
	requireRecipient(secondSecret, "bob")
	request(firstSecret, "/notify", `{"content":"alice report"}`, http.StatusOK)
	request(secondSecret, "/notify", `{"content":"bob report"}`, http.StatusOK)
	supersededResult, err := fixture.service.keys.IssuePairing(t.Context(), first.ID)
	superseded := supersededResult.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	replacementResult, err := fixture.service.keys.IssuePairing(t.Context(), first.ID)
	replacement := replacementResult.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	// Issuing a replacement code leaves the current recipient active.
	requireRecipient(firstSecret, "alice")
	request(firstSecret, "/notify", `{"content":"before rebind"}`, http.StatusOK)
	if err := fixture.deliverMessage(t, "intruder", "bind "+superseded); err != nil {
		t.Fatal(err)
	}
	requireRecipient(firstSecret, "alice")
	if err := fixture.deliverMessage(t, "charlie", "bind "+replacement); err != nil {
		t.Fatal(err)
	}
	requireRecipient(firstSecret, "charlie")
	requireRecipient(secondSecret, "bob")
	fixture.server.Close()
	if err := fixture.service.keys.Close(); err != nil {
		t.Fatal(err)
	}
	// No JSON recipient state or global override is consulted on restart.
	if err := os.WriteFile(filepath.Join(fixture.settings.DataDir, "recipient.json"), []byte("invalid obsolete state"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := fixture.assemble(t)
	fixture.service = restarted
	fixture.server = httptest.NewServer(restarted.routes())
	requireRecipient(firstSecret, "charlie")
	requireRecipient(secondSecret, "bob")
	request(firstSecret, "/notify", `{"content":"after restart"}`, http.StatusOK)
	request(secondSecret, "/notify", `{"content":"bob after restart"}`, http.StatusOK)
	for index, recipient := range []string{"alice", "bob", "alice", "charlie", "bob"} {
		if fixture.messages[index]["recipient"] != recipient {
			t.Fatalf("notification %d delivered to incorrect recipient", index)
		}
	}
	if fixture.sendCalls.Load() != 5 {
		t.Fatal("invalid requests reached QQ")
	}
	entries, err := fixture.service.keys.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]apikey.Key{}
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	if byID[first.ID].UserOpenID != "charlie" || byID[second.ID].UserOpenID != "bob" || byID[first.ID].PairingExpiresAt != nil {
		t.Fatal("SQLite did not retain isolated bindings or clear consumed code")
	}
	for _, secret := range []string{firstSecret, secondSecret, firstCode, secondCode, superseded, replacement, "alice report", "bob report"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credential or notification appeared in logs")
		}
	}
	bound := 0
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "recipient_paired" {
			bound++
			if event["api_key_id"] == nil || event["recipient_ref"] == nil || event["connection_id"] == nil {
				t.Fatal("binding lost key/gateway correlation")
			}
		}
	}
	if bound != 3 {
		t.Fatalf("binding state changes=%d, want three", bound)
	}
}

func TestBehaviorExpiredAndRevokedPairingCodesCannotBind(t *testing.T) {
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
	created, err := fixture.service.keys.Create(t.Context(), "expired")
	key, secret, code := created.Key, created.APIKey, created.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	// Force expiration through the persisted state so the production SQL deadline
	// check is exercised without waiting fifteen minutes or replacing the clock.
	if _, err := database.ExecContext(t.Context(), "UPDATE api_keys SET pairing_expires_at = ? WHERE id = ?", time.Now().UTC().Format(time.RFC3339), key.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "alice", "bind "+code); err != nil {
		t.Fatal(err)
	}
	status, result, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + secret})
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != false {
		t.Fatal("expired code bound a recipient")
	}
	if _, err := fixture.service.keys.Revoke(t.Context(), key.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "alice", "bind "+code); err != nil {
		t.Fatal(err)
	}
	revokedCreated, err := fixture.service.keys.Create(t.Context(), "revoked")
	revokedSecret, revokedCode := revokedCreated.APIKey, revokedCreated.PairingCode
	if err != nil {
		t.Fatal(err)
	}
	revoked, valid, err := fixture.service.keys.Authenticate(t.Context(), revokedSecret)
	if err != nil || !valid {
		t.Fatal("new key could not be read")
	}
	if _, err := fixture.service.keys.Revoke(t.Context(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.deliverMessage(t, "bob", "bind "+revokedCode); err != nil {
		t.Fatal(err)
	}
	listed, err := fixture.service.keys.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listed {
		if (entry.ID == key.ID || entry.ID == revoked.ID) && (entry.UserOpenID != "" || entry.PairingExpiresAt != nil) {
			t.Fatal("revoked code remained bindable")
		}
	}
	status, _, _ = fixture.request(t, http.MethodPost, "/notify", `{"content":"revoked"}`, map[string]string{"Authorization": "Bearer " + revokedSecret})
	requireStatus(t, status, http.StatusUnauthorized)
	if fixture.sendCalls.Load() != 0 {
		t.Fatal("rejected bindings sent notifications")
	}
}

func TestBehaviorConcurrentPairingConsumesCodeOnce(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	left, err := apikey.Open(t.Context(), fixture.settings.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := left.Close(); err != nil {
			t.Error(err)
		}
	}()
	right, err := apikey.Open(t.Context(), fixture.settings.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := right.Close(); err != nil {
			t.Error(err)
		}
	}()
	start := make(chan struct{})
	type result struct {
		key     apikey.Key
		changed bool
		err     error
	}
	outcomes := make(chan result, 2)
	for index, store := range []*apikey.Store{left, right} {
		go func(index int, store *apikey.Store) {
			<-start
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			user := []string{"alice", "bob"}[index]
			key, changed, err := store.Bind(ctx, fixture.pairingCode, user)
			outcomes <- result{key, changed, err}
		}(index, store)
	}
	close(start)
	var winner string
	successes := 0
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if outcome.changed {
			successes++
			winner = outcome.key.UserOpenID
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent code consumptions=%d, want one", successes)
	}
	status, state, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	if state["user_openid"] != winner {
		t.Fatal("status did not select the winning binding")
	}
	status, _, _ = fixture.notify(t, `{"content":"one recipient"}`)
	requireStatus(t, status, http.StatusOK)
	if fixture.messages[0]["recipient"] != winner {
		t.Fatal("notification did not select the winning recipient")
	}
}

type lockedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedLogBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}
func (buffer *lockedLogBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
