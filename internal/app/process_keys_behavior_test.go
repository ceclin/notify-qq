package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testProcessAPIKeys(t *testing.T, binary string) {
	process := launchServiceProcess(t, binary, map[string]string{"API_PATH_PREFIX": "/api", "NOTIFY_API_KEY": strings.Repeat("k", 32), "QQ_USER_OPENID": "obsolete-global", "PAIRING_CODE": "obsolete"})
	address := process.waitForReady(t)
	type credential struct {
		ID          string `json:"id"`
		Key         string `json:"key"`
		Name        string `json:"name"`
		PairingCode string `json:"pairing_code"`
	}
	create := func(name string) credential {
		output := runKeyCommand(t, binary, process.command.Dir, []string{"keys", "create", "--name", name}, 0)
		var result credential
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatal(err)
		}
		if result.ID == "" || len(result.Key) != 47 || len(result.PairingCode) != 47 || result.Key == result.PairingCode || result.Name != name {
			t.Fatal("create did not return a named credential")
		}
		return result
	}
	first, second := create("backup"), create("deploy")
	if first.Key == second.Key {
		t.Fatal("generated keys were identical")
	}
	requestStatus := func(secret string, expected int) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address+"/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		client := &http.Client{Timeout: time.Second}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != expected {
			t.Fatalf("stored key HTTP status = %d, want %d", response.StatusCode, expected)
		}
		if expected == http.StatusOK {
			var state struct {
				Paired     bool   `json:"paired"`
				UserOpenID string `json:"user_openid"`
			}
			if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
				t.Fatal(err)
			}
			if state.Paired || state.UserOpenID != "" {
				t.Fatal("new key inherited a global or another key's recipient")
			}
		}

	}
	requestStatus(strings.Repeat("k", 32), http.StatusUnauthorized)
	requestStatus(first.Key, http.StatusOK)
	requestStatus(second.Key, http.StatusOK)
	pairedOutput := runKeyCommand(t, binary, process.command.Dir, []string{"keys", "pair", first.ID}, 0)
	var paired struct {
		PairingCode      string `json:"pairing_code"`
		UserOpenID       string `json:"user_openid"`
		PairingExpiresAt string `json:"pairing_expires_at"`
	}
	if err := json.Unmarshal(pairedOutput, &paired); err != nil {
		t.Fatal(err)
	}
	if paired.PairingCode == first.PairingCode || len(paired.PairingCode) != 47 || paired.PairingExpiresAt == "" || paired.UserOpenID != "" {
		t.Fatal("pair did not issue a fresh code for an unbound key")
	}
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "revoke", first.ID}, 0)
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "pair", first.ID}, 1)
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "pair", "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, 1)
	requestStatus(first.Key, http.StatusUnauthorized)
	requestStatus(second.Key, http.StatusOK)
	duplicate := runKeyCommand(t, binary, process.command.Dir, []string{"keys", "revoke", first.ID}, 0)
	if !bytes.Contains(duplicate, []byte(`"changed":false`)) {
		t.Fatal("repeated revocation was not idempotent")
	}
	listed := runKeyCommand(t, binary, process.command.Dir, []string{"keys", "list"}, 0)
	var entries []struct {
		ID        string  `json:"id"`
		RevokedAt *string `json:"revoked_at"`
	}
	if err := json.Unmarshal(listed, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("listed keys=%d, want fixture plus two scripts", len(entries))
	}
	revoked := false
	for _, entry := range entries {
		if entry.ID == first.ID {
			revoked = entry.RevokedAt != nil
		}
	}
	if !revoked {
		t.Fatal("revoked metadata was not retained")
	}
	for _, data := range [][]byte{listed, mustReadProcessFile(t, process.logPath)} {
		if bytes.Contains(data, []byte(first.Key)) || bytes.Contains(data, []byte(second.Key)) || bytes.Contains(data, []byte(first.PairingCode)) || bytes.Contains(data, []byte(second.PairingCode)) || bytes.Contains(data, []byte(paired.PairingCode)) {
			t.Fatal("key appeared in listing or logs")
		}
	}
	info, err := os.Stat(filepath.Join(process.command.Dir, "notify.sqlite3"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("SQLite permissions are not 0600: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(process.command.Dir, "notify.sqlite3"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{first.Key, second.Key, first.PairingCode, second.PairingCode, paired.PairingCode, process.apiKey, strings.Repeat("k", 32)} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatal("plaintext key persisted to SQLite")
			}
		}
	}
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "revoke", "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, 1)
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "create", "--name", ""}, 1)
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "create", "--name", "bad\nname"}, 1)
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "revoke", "invalid"}, 1)
	runKeyCommand(t, binary, process.command.Dir, []string{"keys", "list", "unexpected"}, 1)
	// CLI credentials persist independently of QQ configuration.
	local := t.TempDir()
	empty := runKeyCommand(t, binary, local, []string{"keys", "list"}, 0)
	if string(empty) != "[]\n" {
		t.Fatal("empty key listing did not return an empty array")
	}
	created := runKeyCommand(t, binary, local, []string{"keys", "create", "--name", "first"}, 0)
	var initial credential
	if err := json.Unmarshal(created, &initial); err != nil {
		t.Fatal(err)
	}
	// A newly started service accepts credentials created before startup.
	restarted := launchServiceProcess(t, binary, map[string]string{"DATA_DIR": local})
	var restartAddress string
	waitForProcessCondition(t, func() bool {
		for _, event := range restarted.events(t) {
			if event["event"] == "service_started" {
				restartAddress = "http://" + event["listen_addr"].(string)
			}
		}
		return restartAddress != ""
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, restartAddress+"/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+initial.Key)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("service rejected a pre-created credential: %d", response.StatusCode)
	}
	restarted.terminate(t)
	if err := restarted.wait(); err != nil {
		t.Fatal(err)
	}
	// An invalid database must cause a nonzero CLI exit without leaking an error to stdout.
	invalid := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalid, "notify.sqlite3"), []byte("invalid database"), 0600); err != nil {
		t.Fatal(err)
	}
	runKeyCommand(t, binary, invalid, []string{"keys", "list"}, 1)
	process.terminate(t)
	if err := process.wait(); err != nil {
		t.Fatal(err)
	}
	process.requireEvents(t, "service_stopped")
}

func mustReadProcessFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runKeyCommand(t *testing.T, binary, directory string, arguments []string, expectedExit int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = directory
	overrides := map[string]string{"GORACE": "atexit_sleep_ms=0", "DATA_DIR": directory, "LOG_LEVEL": "debug", "QQ_APP_ID": "", "QQ_APP_SECRET": "", "NOTIFY_API_KEY": "", "PAIRING_CODE": "", "QQ_USER_OPENID": "", "LISTEN_ADDR": "invalid", "API_PATH_PREFIX": "invalid"}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[name]; !overridden {
			command.Env = append(command.Env, entry)
		}
	}
	for name, value := range overrides {
		command.Env = append(command.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	actualExit := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("key command execution failed: %v", err)
		}
		actualExit = exitError.ExitCode()
	}
	if actualExit != expectedExit {
		t.Fatalf("key command exit=%d want=%d, stderr=%s", actualExit, expectedExit, stderr.String())
	}
	decoder := json.NewDecoder(&stderr)
	failures := 0
	for {
		var event map[string]any
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if event["level"] == "ERROR" {
			failures++
			if event["event"] != "command_failed" {
				t.Fatal("CLI error logged at wrong boundary")
			}
		}
	}
	if expectedExit != 0 && (failures != 1 || stdout.Len() != 0) {
		t.Fatal("failed command should emit one error and no stdout")
	}
	if expectedExit == 0 && failures != 0 {
		t.Fatal("successful CLI emitted an error")
	}
	if bytes.Contains(stdout.Bytes(), []byte(`"key":`)) || bytes.Contains(stdout.Bytes(), []byte(`"pairing_code":`)) {
		var result map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"key", "pairing_code"} {
			if secret, ok := result[field].(string); ok && bytes.Contains(stderr.Bytes(), []byte(secret)) {
				t.Fatal("generated credential appeared in stderr")
			}
		}
	}
	return stdout.Bytes()
}
