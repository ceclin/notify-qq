package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/samber/oops"

	"github.com/ceclin/notify-qq/internal/apikey"
	"github.com/ceclin/notify-qq/internal/config"
	"github.com/ceclin/notify-qq/internal/logging"
)

const commandHelp = `Usage:
  notify-qq                         Start the HTTP service and QQ WebSocket worker
  notify-qq --version               Show version, source commit, and build metadata as JSON
  notify-qq keys create --name NAME  Generate a script credential (JSON, shown once)
  notify-qq keys pair ID              Issue a fresh 15-minute code to bind/rebind a recipient
  notify-qq keys list                List key metadata without secrets
  notify-qq keys revoke ID           Revoke a credential by ID

Key commands use DATA_DIR and LOG_LEVEL from .env/environment and need no QQ credentials.
`

// RunCommand keeps signals and exit status in cmd while sharing configuration,
// SQLite access, and one-time error reporting with the server application.
func RunCommand(ctx context.Context, arguments []string, output io.Writer) (commandError error) {
	if len(arguments) == 0 {
		return Run(ctx)
	}
	commandID := ulid.Make().String()
	logger := slog.New(logging.NewLogger(os.Stderr, slog.LevelInfo).Handler().WithAttrs([]slog.Attr{slog.String("command_id", commandID)}))
	ctx = logging.WithLogger(ctx, logger)
	started := time.Now()
	defer func() {
		if commandError != nil {
			logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "Local command failed", slog.String("event", "command_failed"), slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))), logging.ErrorAttribute(commandError))
		}
	}()
	if len(arguments) == 1 && arguments[0] == "--version" {
		return writeVersion(output)
	}
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		_, err := io.WriteString(output, commandHelp)
		return oops.In("command").Wrap(err)
	}
	if len(arguments) < 2 || arguments[0] != "keys" {
		return oops.In("command").Code("command_invalid").New("use keys create, keys pair, keys list, or keys revoke; see --help")
	}
	operation := arguments[1]
	var name, id string
	switch operation {
	case "create":
		flags := flag.NewFlagSet("keys create", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&name, "name", "", "script name")
		if err := flags.Parse(arguments[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				_, err = io.WriteString(output, commandHelp)
				return oops.In("command").Wrap(err)
			}
			return oops.In("command").Code("command_invalid").New("invalid create arguments; use keys create --name NAME")
		}
		if flags.NArg() != 0 || strings.TrimSpace(name) == "" {
			return oops.In("command").Code("command_invalid").New("use keys create --name NAME")
		}
	case "list":
		if len(arguments) != 2 {
			return oops.In("command").Code("command_invalid").New("use keys list without arguments")
		}
	case "pair", "revoke":
		if len(arguments) != 3 {
			return oops.In("command").Code("command_invalid").New("use keys pair ID or keys revoke ID")
		}
		id = arguments[2]
		if _, err := ulid.ParseStrict(id); err != nil {
			return oops.In("command").Code("command_invalid").New("key ID must be a ULID returned by keys create or list")
		}
	default:
		return oops.In("command").Code("command_invalid").New("unknown keys operation; see --help")
	}
	settings, err := config.LoadLocal()
	if err != nil {
		return err
	}
	logger = slog.New(logging.NewLogger(os.Stderr, settings.LogLevel).Handler().WithAttrs([]slog.Attr{slog.String("command_id", commandID), slog.String("operation", "keys_"+operation)}))
	ctx = logging.WithLogger(ctx, logger)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, err := apikey.Open(ctx, settings.DataDir)
	if err != nil {
		return err
	}
	defer func() { commandError = errors.Join(commandError, store.Close()) }()
	reportMigrations(ctx, store)
	var result any
	switch operation {
	case "create":
		created, err := store.Create(ctx, name)
		if err != nil {
			return err
		}
		result = created
		logger.LogAttrs(ctx, slog.LevelInfo, "API key created", slog.String("event", "api_key_created"), slog.String("api_key_id", created.ID), slog.String("api_key_name", created.Name), slog.String("pairing_expires_at", *created.PairingExpiresAt))
	case "pair":
		paired, err := store.IssuePairing(ctx, id)
		if err != nil {
			return err
		}
		result = paired
		logger.LogAttrs(ctx, slog.LevelInfo, "Pairing code issued", slog.String("event", "pairing_code_issued"), slog.String("api_key_id", paired.ID), slog.String("pairing_expires_at", *paired.PairingExpiresAt))
	case "list":
		keys, err := store.List(ctx)
		if err != nil {
			return err
		}
		result = keys
		logger.LogAttrs(ctx, slog.LevelInfo, "API keys listed", slog.String("event", "api_keys_listed"), slog.Int("key_count", len(keys)))
	case "revoke":
		changed, err := store.Revoke(ctx, id)
		if err != nil {
			return err
		}
		result = struct {
			ID      string `json:"id"`
			Revoked bool   `json:"revoked"`
			Changed bool   `json:"changed"`
		}{id, true, changed}
		logger.LogAttrs(ctx, slog.LevelInfo, "API key revoked", slog.String("event", "api_key_revoked"), slog.String("api_key_id", id), slog.Bool("changed", changed))
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return oops.In("command").Code("command_output_failed").Wrapf(err, "command result could not be written")
	}
	logger.LogAttrs(ctx, slog.LevelDebug, "Local command completed", slog.String("event", "command_completed"), slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))))
	return nil
}
