package telemetry

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/samber/oops"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const exportTimeout = 3 * time.Second

type Config struct {
	Endpoint    string
	ServiceName string
	SampleRatio float64
	Headers     map[string]string
}

type Runtime struct {
	traces   *sdktrace.TracerProvider
	metrics  *sdkmetric.MeterProvider
	tracer   trace.Tracer
	requests metric.Float64Histogram
	qqCalls  metric.Float64Histogram
}

// An empty endpoint keeps local runs independent of an observability backend.
// Providers belong to the application rather than the process-wide OTEL globals.
func New(ctx context.Context, settings Config, version string) (*Runtime, error) {
	if settings.Endpoint == "" {
		return nil, nil
	}
	identity := resource.NewSchemaless(
		attribute.String("service.name", settings.ServiceName),
		attribute.String("service.version", version),
	)
	traces, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(settings.Endpoint+"/v1/traces"),
		otlptracehttp.WithHeaders(settings.Headers),
		otlptracehttp.WithTimeout(exportTimeout),
	)
	if err != nil {
		return nil, oops.In("telemetry").Code("telemetry_start_failed").Wrapf(err, "trace exporter could not start")
	}
	runtime := &Runtime{traces: sdktrace.NewTracerProvider(
		sdktrace.WithResource(identity),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(settings.SampleRatio))),
		sdktrace.WithBatcher(traces, sdktrace.WithMaxQueueSize(1024), sdktrace.WithMaxExportBatchSize(256),
			sdktrace.WithBatchTimeout(5*time.Second), sdktrace.WithExportTimeout(exportTimeout)),
	)}
	cleanup := func(cause error) (*Runtime, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exportTimeout)
		defer cancel()
		return nil, errors.Join(cause, runtime.Shutdown(cleanupCtx))
	}
	metrics, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(settings.Endpoint+"/v1/metrics"),
		otlpmetrichttp.WithHeaders(settings.Headers),
		otlpmetrichttp.WithTimeout(exportTimeout),
	)
	if err != nil {
		return cleanup(oops.In("telemetry").Code("telemetry_start_failed").Wrapf(err, "metric exporter could not start"))
	}
	runtime.metrics = sdkmetric.NewMeterProvider(sdkmetric.WithResource(identity), sdkmetric.WithCardinalityLimit(256),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics,
			sdkmetric.WithInterval(30*time.Second), sdkmetric.WithTimeout(exportTimeout))),
	)
	runtime.tracer = runtime.traces.Tracer("github.com/ceclin/notify-qq")
	meter := runtime.metrics.Meter("github.com/ceclin/notify-qq")
	boundaries := metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30)
	runtime.requests, err = meter.Float64Histogram("http.server.request.duration", metric.WithUnit("s"), boundaries)
	if err != nil {
		return cleanup(oops.In("telemetry").Code("telemetry_start_failed").Wrapf(err, "HTTP metric could not start"))
	}
	runtime.qqCalls, err = meter.Float64Histogram("qq.client.request.duration", metric.WithUnit("s"), boundaries)
	if err != nil {
		return cleanup(oops.In("telemetry").Code("telemetry_start_failed").Wrapf(err, "QQ metric could not start"))
	}
	return runtime, nil
}

func (runtime *Runtime) Shutdown(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	traceError := runtime.traces.Shutdown(ctx)
	var metricError error
	if runtime.metrics != nil {
		metricError = runtime.metrics.Shutdown(ctx)
	}
	return oops.In("telemetry").Code("telemetry_shutdown_failed").Wrapf(errors.Join(traceError, metricError), "telemetry could not finish exporting")
}

func (runtime *Runtime) StartRequest(ctx context.Context, headers http.Header, method, route string) (context.Context, func(int)) {
	if runtime == nil {
		return ctx, func(int) {}
	}
	ctx = (propagation.TraceContext{}).Extract(ctx, propagation.HeaderCarrier(headers))
	if route == "" {
		route = "unmatched"
	}
	method = requestMethod(method)
	attributes := []attribute.KeyValue{attribute.String("http.request.method", method), attribute.String("http.route", route)}
	ctx, span := runtime.tracer.Start(ctx, method+" "+route, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attributes...))
	started := time.Now()
	return ctx, func(status int) {
		attributes = append(attributes, attribute.Int("http.response.status_code", status))
		span.SetAttributes(attributes...)
		if status >= 500 {
			span.SetStatus(codes.Error, "HTTP request failed")
		}
		runtime.requests.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(attributes...))
		span.End()
	}
}

// QQ recipient paths and payloads are deliberately absent from telemetry.
func (runtime *Runtime) StartQQCall(ctx context.Context, method, operation string) (context.Context, func(int, bool)) {
	if runtime == nil {
		return ctx, func(int, bool) {}
	}
	attributes := []attribute.KeyValue{
		attribute.String("http.request.method", method), attribute.String("qq.operation", operation),
		attribute.String("server.address", "api.bot.qq.com"),
	}
	ctx, span := runtime.tracer.Start(ctx, "qq."+operation, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attributes...))
	started := time.Now()
	return ctx, func(status int, failed bool) {
		if status > 0 {
			attributes = append(attributes, attribute.Int("http.response.status_code", status))
		}
		result := "success"
		if failed {
			result = "error"
			span.SetStatus(codes.Error, "QQ request failed")
		}
		attributes = append(attributes, attribute.String("result", result))
		span.SetAttributes(attributes...)
		runtime.qqCalls.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(attributes...))
		span.End()
	}
}

func requestMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "_OTHER"
	}
}
