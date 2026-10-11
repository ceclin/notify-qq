package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/telemetry"
	"github.com/ceclin/notify-qq/internal/testsupport"
)

func TestBehaviorTelemetryExportsSafeCorrelatedWorkflows(t *testing.T) {
	t.Parallel()
	collector := testsupport.NewCollector(t, "Bearer collector-private")
	originalTraces, originalMetrics := otel.GetTracerProvider(), otel.GetMeterProvider()
	observability, shutdown := startBehaviorTelemetry(t, collector, 1)
	var logs lockedLogBuffer
	var reject atomic.Bool
	fixture := startBehaviorServiceWithTelemetry(t, "private-recipient", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Traceparent") != "" || r.Header.Get("Baggage") != "" {
			t.Error("internal trace context was sent to the external QQ platform")
		}
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":7200}`)
		} else if reject.Load() {
			_, _ = io.WriteString(w, `{"code":400123,"message":"private-upstream-message"}`)
		} else {
			_, _ = io.WriteString(w, `{"id":"qq-message-id"}`)
		}
	}, "/api", logging.NewLogger(&logs, slog.LevelDebug), observability)
	parentTrace := "0123456789abcdef0123456789abcdef"
	parentSpan := "1234567890abcdef"
	status, _, headers := fixture.request(t, http.MethodPost, "/api/notify?private-query=hidden", `{"content":"private-notification","buttons":[{"label":"secret","url":"https://private.invalid/private-link"}]}`, map[string]string{
		"Authorization": "Bearer " + fixture.apiKey,
		"traceparent":   "00-" + parentTrace + "-" + parentSpan + "-01",
		"baggage":       "private-baggage=never-export",
	})
	requireStatus(t, status, http.StatusOK)
	reject.Store(true)
	status, _, _ = fixture.notify(t, `{"content":"private-rejected-content"}`)
	requireStatus(t, status, http.StatusBadGateway)
	status, _, _ = fixture.notify(t, `{"content":`)
	requireStatus(t, status, http.StatusBadRequest)
	status, _, _ = fixture.request(t, http.MethodGet, "/private-unknown-path?private-query=hidden", "", nil)
	requireStatus(t, status, http.StatusNotFound)
	status, _, _ = fixture.request(t, "CUSTOM", "/api/notify", "", nil)
	requireStatus(t, status, http.StatusMethodNotAllowed)
	for _, path := range []string{"/api/healthz", "/api/readyz"} {
		status, _, _ = fixture.request(t, http.MethodGet, path, "", nil)
		requireStatus(t, status, http.StatusOK)
	}
	fixture.server.Close()
	shutdown()
	snapshot := collector.Snapshot()
	var root *tracepb.Span
	failedClient, failedServer, otherMethod := false, false, false
	for _, span := range snapshot.Spans {
		if span.Kind == tracepb.Span_SPAN_KIND_SERVER && hex.EncodeToString(span.TraceId) == parentTrace {
			root = span
		}
		if span.Name == "qq.send_message" && span.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
			failedClient = true
		}
		if span.Kind == tracepb.Span_SPAN_KIND_SERVER && span.Status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
			failedServer = true
		}
		if telemetryAttribute(span.Attributes, "http.request.method") == "_OTHER" {
			otherMethod = true
		}
	}
	if root == nil || root.Name != "POST /api/notify" || hex.EncodeToString(root.ParentSpanId) != parentSpan || !failedClient || !failedServer || !otherMethod {
		t.Fatal("exported spans missed propagation, normalized names, or error outcomes")
	}
	children := 0
	for _, span := range snapshot.Spans {
		if hex.EncodeToString(span.ParentSpanId) == hex.EncodeToString(root.SpanId) {
			children++
		}
	}
	if children != 2 {
		t.Fatal("successful request did not contain the token and message client spans")
	}
	counts := make(map[string]uint64)
	for _, metric := range snapshot.Metrics {
		if metric.Unit != "s" {
			t.Fatal("request durations were not exported in seconds")
		}
		for _, point := range metric.GetHistogram().GetDataPoints() {
			counts[metric.Name] += point.Count
		}
	}
	if counts["http.server.request.duration"] != 5 || counts["qq.client.request.duration"] != 3 {
		t.Fatalf("unexpected request metric counts: %v", counts)
	}
	for _, resource := range snapshot.Resources {
		if telemetryAttribute(resource.Attributes, "service.name") != "notify-behavior" || telemetryAttribute(resource.Attributes, "service.version") != "behavior" {
			t.Fatal("telemetry resources omitted configured service identity")
		}
	}
	correlated := false
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["request_id"] == headers.Get("X-Request-ID") {
			if event["trace_id"] != parentTrace || len(event["span_id"].(string)) != 16 {
				t.Fatal("request logs lost active trace/span context")
			}
			if event["event"] == "notification_accepted" {
				correlated = event["span_id"] == hex.EncodeToString(root.SpanId)
			}
		}
	}
	if !correlated || otel.GetTracerProvider() != originalTraces || otel.GetMeterProvider() != originalMetrics {
		t.Fatal("telemetry lost log correlation or replaced global providers")
	}
	for _, private := range []string{fixture.apiKey, fixture.pairingCode, "test-secret", "test-token", "private-recipient", "private-notification", "private-link", "private-upstream-message", "private-query", "private-baggage", "private-unknown-path", "collector-private", "CUSTOM"} {
		if strings.Contains(snapshot.Payloads, private) {
			t.Fatal("OTLP payload exposed private data or unbounded HTTP input")
		}
	}
}

func TestBehaviorTelemetryHonorsParentSampling(t *testing.T) {
	for _, scenario := range []struct {
		name, parent string
		ratio        float64
		spans        int
	}{
		{"zero ratio", "", 0, 0},
		{"sampled parent", "00-0123456789abcdef0123456789abcdef-1234567890abcdef-01", 0, 3},
		{"unsampled parent", "00-0123456789abcdef0123456789abcdef-1234567890abcdef-00", 1, 0},
		{"malformed parent", "invalid", 1, 3},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			collector := testsupport.NewCollector(t, "Bearer collector-private")
			observability, shutdown := startBehaviorTelemetry(t, collector, scenario.ratio)
			fixture := startBehaviorServiceWithTelemetry(t, "owner-openid", nil, "", logging.NewLogger(io.Discard, slog.LevelInfo), observability)
			status, _, _ := fixture.request(t, http.MethodPost, "/notify", `{"content":"## Completed"}`, map[string]string{
				"Authorization": "Bearer " + fixture.apiKey, "traceparent": scenario.parent,
			})
			requireStatus(t, status, http.StatusOK)
			fixture.server.Close()
			shutdown()
			snapshot := collector.Snapshot()
			if len(snapshot.Spans) != scenario.spans || len(snapshot.Metrics) != 2 {
				t.Fatal("trace sampling affected metric delivery or ignored the parent decision")
			}
		})
	}
}

func startBehaviorTelemetry(t *testing.T, collector *testsupport.Collector, ratio float64) (*telemetry.Runtime, func()) {
	t.Helper()
	observability, err := telemetry.New(t.Context(), telemetry.Config{
		Endpoint: collector.Endpoint, ServiceName: "notify-behavior", SampleRatio: ratio,
		Headers: map[string]string{"Authorization": "Bearer collector-private"},
	}, "behavior")
	if err != nil {
		t.Fatal(err)
	}
	shutdown := sync.OnceFunc(func() { shutdownBehaviorTelemetry(t, observability) })
	t.Cleanup(shutdown)
	return observability, shutdown
}

func shutdownBehaviorTelemetry(t *testing.T, observability *telemetry.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := observability.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func telemetryAttribute(attributes []*commonpb.KeyValue, name string) string {
	for _, attribute := range attributes {
		if attribute.Key == name {
			return attribute.Value.GetStringValue()
		}
	}
	return ""
}
