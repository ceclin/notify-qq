package app_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ceclin/notify-qq/internal/testsupport"
)

func testProcessTelemetry(t *testing.T, binary string) {
	for _, mode := range []string{"available", "rejected", "unresponsive"} {
		rejected := mode != "available"
		name := "flush on shutdown"
		if rejected {
			name = mode + " collector preserves delivery and exit status"
		}
		t.Run(name, func(t *testing.T) {
			collector := testsupport.NewCollector(t, "Bearer process-collector-private")
			collector.Reject.Store(mode == "rejected")
			collector.Stall.Store(mode == "unresponsive")
			upstream := newProcessQQServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveProcessGatewayRoute(t, w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/app/getAppAccessToken" {
					_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
				} else {
					_, _ = io.WriteString(w, `{"id":"process-message"}`)
				}
			}))
			t.Cleanup(upstream.Close)
			certificatePath := filepath.Join(t.TempDir(), "qq.pem")
			if err := os.WriteFile(certificatePath, processQQCertificate(upstream), 0600); err != nil {
				t.Fatal(err)
			}
			process := launchServiceProcess(t, binary, map[string]string{
				"SSL_CERT_FILE":               certificatePath,
				"OTEL_EXPORTER_OTLP_ENDPOINT": collector.Endpoint,
				"OTEL_SERVICE_NAME":           "notify-process", "OTEL_TRACES_SAMPLER_ARG": "1",
				"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer%20process-collector-private",
			}, upstream)
			address := process.waitForReady(t)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, address+"/notify", strings.NewReader(`{"content":"process-private-notification"}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+process.apiKey)
			request.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: 2 * time.Second}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatal("collector availability changed notification delivery")
			}
			started := time.Now()
			process.terminate(t)
			if err := process.wait(); err != nil || time.Since(started) > 7*time.Second {
				t.Fatal("telemetry prevented bounded successful process shutdown")
			}
			process.requireEvents(t, "telemetry_started", "telemetry_stopped", "service_stopped")
			if collector.Requests.Load() == 0 {
				t.Fatal("process did not export telemetry before exit")
			}
			warnings := 0
			for _, event := range process.events(t) {
				if event["event"] == "telemetry_failed" {
					warnings++
				}
				if event["event"] == "telemetry_stopped" && event["flushed"] != !rejected {
					t.Fatal("shutdown reported an incorrect telemetry outcome")
				}
			}
			if warnings > 1 || rejected && warnings != 1 {
				t.Fatal("SDK errors were missing or repeated")
			}
			logs, err := os.ReadFile(process.logPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"collector-private-response", "process-collector-private", "process-private-notification"} {
				if strings.Contains(string(logs), private) || strings.Contains(collector.Snapshot().Payloads, private) {
					t.Fatal("process exposed collector credentials, errors, or notification text")
				}
			}
			if !rejected {
				snapshot := collector.Snapshot()
				if len(snapshot.Spans) == 0 || len(snapshot.Metrics) != 2 || !strings.Contains(snapshot.Payloads, "notify-process") {
					t.Fatal("shutdown did not flush spans, metrics, and environment-overridden service identity")
				}
			}
		})
	}
	for _, scenario := range []struct{ setting, value string }{
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "https://private:secret@collector.invalid"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.invalid?secret=private"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.invalid/#"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "not-a-url"},
		{"OTEL_TRACES_SAMPLER_ARG", "NaN"},
		{"OTEL_TRACES_SAMPLER_ARG", "1.1"},
		{"OTEL_EXPORTER_OTLP_HEADERS", "Authorization=private%0d%0asecret"},
		{"OTEL_EXPORTER_OTLP_HEADERS", "private-header-without-equals"},
		{"OTEL_SERVICE_NAME", ""},
	} {
		t.Run("reject invalid "+scenario.setting+" "+scenario.value, func(t *testing.T) {
			settings := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OTEL_TRACES_SAMPLER_ARG": "1"}
			settings[scenario.setting] = scenario.value
			process := launchServiceProcess(t, binary, settings)
			process.requireStartupFailure(t)
			logs, err := os.ReadFile(process.logPath)
			if err != nil || strings.Contains(string(logs), scenario.value) && scenario.value != "" {
				t.Fatal("invalid telemetry configuration exposed its value in startup logs")
			}
		})
	}
}
