package config

import (
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/samber/oops"
	"github.com/spf13/viper"
	"golang.org/x/net/http/httpguts"

	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/telemetry"
)

func telemetrySettings(settings *viper.Viper) (telemetry.Config, error) {
	c := telemetry.Config{Endpoint: strings.TrimSpace(settings.GetString("OTEL_EXPORTER_OTLP_ENDPOINT"))}
	if c.Endpoint == "" {
		return c, nil
	}
	invalid := func(setting, message string) error {
		return logging.WithErrorAttributes(oops.In("config").Code("config_invalid"), slog.String("setting", setting)).New(message)
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil || endpoint.Hostname() == "" || endpoint.Scheme != "http" && endpoint.Scheme != "https" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(c.Endpoint, "#") || endpoint.Opaque != "" {
		return c, invalid("OTEL_EXPORTER_OTLP_ENDPOINT", "OTLP endpoint must be an HTTP(S) URL without credentials, query, or fragment")
	}
	c.Endpoint = strings.TrimRight(c.Endpoint, "/")
	c.ServiceName = strings.TrimSpace(settings.GetString("OTEL_SERVICE_NAME"))
	if c.ServiceName == "" {
		return c, invalid("OTEL_SERVICE_NAME", "telemetry service name is required")
	}
	c.SampleRatio, err = strconv.ParseFloat(settings.GetString("OTEL_TRACES_SAMPLER_ARG"), 64)
	if err != nil || math.IsNaN(c.SampleRatio) || c.SampleRatio < 0 || c.SampleRatio > 1 {
		return c, invalid("OTEL_TRACES_SAMPLER_ARG", "trace sampling ratio must be between zero and one")
	}
	c.Headers = make(map[string]string)
	for entry := range strings.SplitSeq(settings.GetString("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		name, value, found := strings.Cut(strings.TrimSpace(entry), "=")
		value, err = url.PathUnescape(value)
		if !found || err != nil || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return c, invalid("OTEL_EXPORTER_OTLP_HEADERS", "OTLP headers must contain valid name=value pairs")
		}
		c.Headers[name] = value
	}
	return c, nil
}
