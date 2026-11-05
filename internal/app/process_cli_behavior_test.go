package app_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testProcessCLI(t *testing.T, binary string) {
	t.Run("help does not start the service or open storage", func(t *testing.T) {
		directory := t.TempDir()
		rootHelp := runLocalCommand(t, binary, directory, []string{"--help"}, 0)
		for _, arguments := range [][]string{nil, {"-h"}, {"help"}} {
			if output := runLocalCommand(t, binary, directory, arguments, 0); string(output) != string(rootHelp) {
				t.Fatalf("%v did not show root help: %s", arguments, output)
			}
		}
		for _, name := range []string{"serve", "healthcheck", "keys"} {
			if !strings.Contains(string(rootHelp), name) {
				t.Fatalf("root help omitted %s", name)
			}
		}
		for _, arguments := range [][]string{
			{"serve", "--help"}, {"help", "serve"}, {"healthcheck", "--help"},
			{"keys"}, {"keys", "--help"}, {"keys", "create", "--help"},
			{"keys", "list", "--help"}, {"keys", "pair", "--help"}, {"keys", "revoke", "--help"},
		} {
			output := runLocalCommand(t, binary, directory, arguments, 0)
			name := arguments[0]
			if name == "help" {
				name = arguments[1]
			}
			if !strings.Contains(string(output), "Usage:") || !strings.Contains(string(output), "notify-qq "+name) {
				t.Fatalf("%v did not show command help: %s", arguments, output)
			}
		}
		version := runLocalCommand(t, binary, directory, []string{"--version"}, 0)
		if json.Valid(version) || !strings.Contains(string(version), "notify-qq dev") || !strings.Contains(string(version), "Go version:") {
			t.Fatalf("default version output is not readable: %s", version)
		}
		if _, err := os.Stat(filepath.Join(directory, "notify.sqlite3")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("help or version opened application storage")
		}
	})

	t.Run("invalid arguments fail before opening storage", func(t *testing.T) {
		directory := t.TempDir()
		for _, arguments := range [][]string{
			{"unknown"}, {"serve", "extra"}, {"serve", "--unknown"}, {"healthcheck", "extra"},
			{"keys", "unknown"}, {"keys", "create"}, {"keys", "create", "--name"},
			{"keys", "create", "--name", " "}, {"keys", "create", "--name", "backup", "extra"},
			{"keys", "list", "extra"}, {"keys", "pair"}, {"keys", "pair", "invalid"},
			{"keys", "revoke", "invalid"}, {"keys", "list", "--unknown"},
		} {
			runLocalCommand(t, binary, directory, arguments, 1)
		}
		if _, err := os.Stat(filepath.Join(directory, "notify.sqlite3")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("argument validation opened application storage")
		}
	})

	t.Run("readable key lifecycle and explicit JSON output", func(t *testing.T) {
		directory := t.TempDir()
		created := runLocalCommand(t, binary, directory, []string{"keys", "create", "--name", "certimate"}, 0)
		fields := cliFields(created)
		id, key, code := fields["ID"], fields["API key"], fields["Pairing code"]
		if json.Valid(created) || id == "" || fields["Name"] != "certimate" || !strings.HasPrefix(key, "nqq_") || !strings.HasPrefix(code, "nqp_") || fields["Expires at"] == "" {
			t.Fatal("default create output is missing readable credential fields")
		}
		listed := runLocalCommand(t, binary, directory, []string{"keys", "list"}, 0)
		for _, value := range []string{"ID", "NAME", "STATUS", "RECIPIENT", id, "certimate", "active", "unpaired"} {
			if !strings.Contains(string(listed), value) {
				t.Fatalf("key table omitted %q", value)
			}
		}
		if strings.Contains(string(listed), key) || strings.Contains(string(listed), code) {
			t.Fatal("key listing exposed credentials")
		}
		paired := runLocalCommand(t, binary, directory, []string{"keys", "pair", id}, 0)
		pairing := cliFields(paired)
		if pairing["ID"] != id || pairing["Pairing code"] == code || !strings.HasPrefix(pairing["Pairing code"], "nqp_") || strings.Contains(string(paired), key) {
			t.Fatal("pairing output did not show a new code without the API key")
		}
		revoked := runLocalCommand(t, binary, directory, []string{"keys", "revoke", id}, 0)
		if !strings.Contains(string(revoked), "Revoked API key "+id) {
			t.Fatalf("revocation did not confirm the key: %s", revoked)
		}
		duplicate := runLocalCommand(t, binary, directory, []string{"keys", "revoke", id}, 0)
		if !strings.Contains(string(duplicate), "already revoked") {
			t.Fatalf("repeated revocation was not explained: %s", duplicate)
		}
		active := runLocalCommand(t, binary, directory, []string{"keys", "list"}, 0)
		if strings.Contains(string(active), id) || !strings.Contains(string(active), "No API keys found.") {
			t.Fatalf("default listing did not hide the revoked key: %s", active)
		}
		all := runLocalCommand(t, binary, directory, []string{"keys", "list", "--all"}, 0)
		if !strings.Contains(string(all), id) || !strings.Contains(string(all), "revoked") {
			t.Fatalf("--all did not show the revoked key: %s", all)
		}
		activeJSON := runLocalCommand(t, binary, directory, []string{"keys", "list", "--json"}, 0)
		if string(activeJSON) != "[]\n" {
			t.Fatalf("JSON listing did not filter revoked keys: %s", activeJSON)
		}
		allJSON := runLocalCommand(t, binary, directory, []string{"--json", "keys", "list", "--all"}, 0)
		var entries []struct {
			ID        string  `json:"id"`
			RevokedAt *string `json:"revoked_at"`
		}
		if err := json.Unmarshal(allJSON, &entries); err != nil || len(entries) != 1 || entries[0].ID != id || entries[0].RevokedAt == nil {
			t.Fatal("--json --all did not retain revocation metadata")
		}
	})
}

func cliFields(output []byte) map[string]string {
	fields := make(map[string]string)
	for line := range strings.SplitSeq(string(output), "\n") {
		if label, value, ok := strings.Cut(line, ":"); ok {
			fields[label] = strings.TrimSpace(value)
		}
	}
	return fields
}
