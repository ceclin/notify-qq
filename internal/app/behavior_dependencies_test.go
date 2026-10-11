package app

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/ceclin/notify-qq/internal/logging"
)

func TestBehaviorServicesKeepInjectedLoggersIsolated(t *testing.T) {
	original := slog.Default()
	for _, prefix := range []string{"/first", "/second"} {
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()
			var logs lockedLogBuffer
			logger := logging.NewLogger(&logs, slog.LevelDebug)
			fixture := startBehaviorServiceWithPrefixAndLogger(t, "owner-openid", nil, prefix, logger)
			status, _, headers := fixture.notify(t, `{"content":"## Private report"}`)
			requireStatus(t, status, 200)
			if slog.Default() != original {
				t.Fatal("service changed the process logger")
			}
			events := 0
			decoder := json.NewDecoder(strings.NewReader(logs.String()))
			for decoder.More() {
				var event map[string]any
				if err := decoder.Decode(&event); err != nil {
					t.Fatal(err)
				}
				if event["route"] != prefix+"/notify" || event["request_id"] != headers.Get("X-Request-ID") {
					t.Fatal("another service's events reached this logger")
				}
				if event["event"] == "notification_accepted" {
					events++
				}
			}
			if events != 1 {
				t.Fatal("injected logger missed the notification outcome")
			}
		})
	}
}
