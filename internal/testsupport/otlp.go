package testsupport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	collectormetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type TelemetrySnapshot struct {
	Spans     []*tracepb.Span
	Metrics   []*metricspb.Metric
	Resources []*resourcepb.Resource
	Payloads  string
}

type Collector struct {
	Endpoint string
	Reject   atomic.Bool
	Stall    atomic.Bool
	Requests atomic.Int64
	mutex    sync.Mutex
	snapshot TelemetrySnapshot
}

// Decode the actual OTLP wire format so behavior tests cover SDK exporters,
// endpoint paths, authentication, and shutdown rather than an in-memory adapter.
func NewCollector(t testing.TB, authorization string) *Collector {
	t.Helper()
	collector := &Collector{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		collector.Requests.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Authorization") != authorization {
			t.Error("OTLP exporter used an incorrect method, encoding, or authorization")
			http.Error(w, "rejected", http.StatusBadRequest)
			return
		}
		if collector.Reject.Load() {
			http.Error(w, "collector-private-response", http.StatusUnauthorized)
			return
		}
		payload, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			return
		}
		if collector.Stall.Load() {
			select {
			case <-r.Context().Done():
			case <-t.Context().Done():
			}
			return
		}
		collector.mutex.Lock()
		defer collector.mutex.Unlock()
		collector.snapshot.Payloads += string(payload)
		switch r.URL.Path {
		case "/collector/v1/traces":
			var request collectortrace.ExportTraceServiceRequest
			if err := proto.Unmarshal(payload, &request); err != nil {
				t.Error(err)
				return
			}
			for _, resource := range request.ResourceSpans {
				collector.snapshot.Resources = append(collector.snapshot.Resources, resource.Resource)
				for _, scope := range resource.ScopeSpans {
					collector.snapshot.Spans = append(collector.snapshot.Spans, scope.Spans...)
				}
			}
		case "/collector/v1/metrics":
			var request collectormetric.ExportMetricsServiceRequest
			if err := proto.Unmarshal(payload, &request); err != nil {
				t.Error(err)
				return
			}
			for _, resource := range request.ResourceMetrics {
				collector.snapshot.Resources = append(collector.snapshot.Resources, resource.Resource)
				for _, scope := range resource.ScopeMetrics {
					collector.snapshot.Metrics = append(collector.snapshot.Metrics, scope.Metrics...)
				}
			}
		default:
			t.Error("OTLP exporter ignored the collector base path")
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	collector.Endpoint = server.URL + "/collector"
	t.Cleanup(server.Close)
	return collector
}

func (collector *Collector) Snapshot() TelemetrySnapshot {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()
	return TelemetrySnapshot{
		Spans:     append([]*tracepb.Span(nil), collector.snapshot.Spans...),
		Metrics:   append([]*metricspb.Metric(nil), collector.snapshot.Metrics...),
		Resources: append([]*resourcepb.Resource(nil), collector.snapshot.Resources...),
		Payloads:  collector.snapshot.Payloads,
	}
}
