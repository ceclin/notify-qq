package config

import (
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/samber/oops"
	"github.com/spf13/viper"

	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/telemetry"
)

type Config struct {
	AppID, AppSecret    string
	ListenAddr, DataDir string
	APIPathPrefix       string
	LogLevel            slog.Level
	Telemetry           telemetry.Config
}

func readSettings() (*viper.Viper, error) {
	settings := viper.New()
	settings.SetConfigFile(".env")
	settings.SetConfigType("env")
	settings.AutomaticEnv()
	// An explicit empty API_PATH_PREFIX must override a file value and select root.
	settings.AllowEmptyEnv(true)
	settings.SetDefault("LISTEN_ADDR", "127.0.0.1:8080")
	settings.SetDefault("API_PATH_PREFIX", "")
	settings.SetDefault("DATA_DIR", "./data")
	settings.SetDefault("LOG_LEVEL", "info")
	settings.SetDefault("OTEL_SERVICE_NAME", "notify-qq")
	settings.SetDefault("OTEL_TRACES_SAMPLER_ARG", "1")
	if err := settings.ReadInConfig(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, oops.In("config").Code("config_read_failed").Wrapf(err, "configuration file could not be read")
	}
	return settings, nil
}

type Local struct {
	DataDir  string
	LogLevel slog.Level
}

// Local commands share the server's file/environment precedence without needing
// QQ credentials, pairing configuration, or a valid listener address.
func LoadLocal() (Local, error) {
	settings, err := readSettings()
	if err != nil {
		return Local{}, err
	}
	return localSettings(settings)
}

func localSettings(settings *viper.Viper) (Local, error) {
	c := Local{DataDir: settings.GetString("DATA_DIR")}
	builder := oops.In("config").Code("config_invalid")
	if strings.TrimSpace(c.DataDir) == "" {
		return c, logging.WithErrorAttributes(builder, slog.String("setting", "DATA_DIR")).New("data directory is required")
	}
	if err := c.LogLevel.UnmarshalText([]byte(settings.GetString("LOG_LEVEL"))); err != nil {
		return c, logging.WithErrorAttributes(builder, slog.String("setting", "LOG_LEVEL")).Wrapf(err, "log level is invalid")
	}
	return c, nil
}

func Load() (Config, error) {
	settings, err := readSettings()
	if err != nil {
		return Config{}, err
	}
	local, err := localSettings(settings)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		AppID: settings.GetString("QQ_APP_ID"), AppSecret: settings.GetString("QQ_APP_SECRET"),
		ListenAddr: settings.GetString("LISTEN_ADDR"), DataDir: local.DataDir, LogLevel: local.LogLevel,
	}
	c.APIPathPrefix, err = normalizeAPIPathPrefix(settings.GetString("API_PATH_PREFIX"))
	if err != nil {
		return c, err
	}
	c.Telemetry, err = telemetrySettings(settings)
	if err != nil {
		return c, err
	}
	builder := oops.In("config").Code("config_invalid")
	for _, required := range []struct{ name, value string }{
		{"QQ_APP_ID", c.AppID}, {"QQ_APP_SECRET", c.AppSecret},
	} {
		if strings.TrimSpace(required.value) == "" {
			return c, logging.WithErrorAttributes(builder, slog.String("setting", required.name)).New("required configuration is missing")
		}
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return c, logging.WithErrorAttributes(builder, slog.String("setting", "LISTEN_ADDR")).Wrapf(err, "listen address is invalid")
	}
	return c, nil
}

// A literal path prevents configuration from registering Echo parameters or
// wildcards. Canonical segments also avoid disagreement with reverse proxies.
func normalizeAPIPathPrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" || prefix == "/" {
		return "", nil
	}
	prefix = strings.TrimSuffix(prefix, "/")
	invalid := logging.WithErrorAttributes(oops.In("config").Code("config_invalid"), slog.String("setting", "API_PATH_PREFIX"))
	if !strings.HasPrefix(prefix, "/") {
		return "", invalid.New("API path prefix must start with a slash")
	}
	for segment := range strings.SplitSeq(prefix[1:], "/") {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsFunc(segment, func(character rune) bool {
			return !isUnreservedPathCharacter(character)
		}) {
			return "", invalid.New("API path prefix must contain literal nonempty URL path segments without dot segments")
		}
	}
	return prefix, nil
}

func isUnreservedPathCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' || strings.ContainsRune("-._~", character)
}
