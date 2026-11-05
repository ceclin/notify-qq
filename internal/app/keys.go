package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/samber/oops"
	"github.com/spf13/cobra"

	"github.com/ceclin/notify-qq/internal/apikey"
	"github.com/ceclin/notify-qq/internal/config"
	"github.com/ceclin/notify-qq/internal/logging"
)

type keyCommandOptions struct {
	name string
	id   string
	all  bool
	json bool
}

func newKeysCommand(commandID string, jsonOutput *bool) *cobra.Command {
	keys := &cobra.Command{
		Use:   "keys",
		Short: "Manage API keys and recipient pairing",
		Long:  "Manage API keys and recipient pairing.\n\nUses DATA_DIR and LOG_LEVEL from .env/environment; QQ credentials are not required.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	var name string
	create := &cobra.Command{
		Use:   "create",
		Short: "Generate a script credential, shown once",
		Args: func(command *cobra.Command, arguments []string) error {
			if err := cobra.NoArgs(command, arguments); err != nil {
				return err
			}
			if strings.TrimSpace(name) == "" {
				return oops.In("command").Code("command_invalid").New("use keys create --name NAME")
			}
			return nil
		},
		RunE: func(command *cobra.Command, _ []string) error {
			return runKeys(command, commandID, keyCommandOptions{name: name, json: *jsonOutput})
		},
	}
	create.Flags().StringVar(&name, "name", "", "Name of the sending script (required)")
	var all bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List active key metadata without secrets",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runKeys(command, commandID, keyCommandOptions{all: all, json: *jsonOutput})
		},
	}
	list.Flags().BoolVar(&all, "all", false, "Include revoked keys")
	keys.AddCommand(create, list)
	for _, operation := range []struct{ name, description string }{
		{"pair", "Issue a fresh 5-minute code to bind or rebind a recipient"},
		{"revoke", "Revoke a credential"},
	} {
		keys.AddCommand(&cobra.Command{
			Use:   operation.name + " KEY_ID",
			Short: operation.description,
			Args: func(command *cobra.Command, arguments []string) error {
				if err := cobra.ExactArgs(1)(command, arguments); err != nil {
					return err
				}
				if _, err := ulid.ParseStrict(arguments[0]); err != nil {
					return oops.In("command").Code("command_invalid").New("key ID must be a ULID returned by keys create or list")
				}
				return nil
			},
			RunE: func(command *cobra.Command, arguments []string) error {
				return runKeys(command, commandID, keyCommandOptions{id: arguments[0], json: *jsonOutput})
			},
		})
	}
	return keys
}

func runKeys(command *cobra.Command, commandID string, options keyCommandOptions) (commandError error) {
	ctx := command.Context()
	operation := command.Name()
	started := time.Now()
	settings, err := config.LoadLocal()
	if err != nil {
		return err
	}
	logger := slog.New(logging.NewLogger(os.Stderr, settings.LogLevel).Handler().WithAttrs([]slog.Attr{slog.String("command_id", commandID), slog.String("operation", "keys_"+operation)}))
	ctx = logging.WithLogger(ctx, logger)
	command.SetContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, err := apikey.Open(ctx, settings.DataDir)
	if err != nil {
		return err
	}
	defer func() { commandError = errors.Join(commandError, store.Close()) }()
	reportMigrations(ctx, store)
	output := command.OutOrStdout()
	var outputError error
	switch operation {
	case "create":
		created, err := store.Create(ctx, options.name)
		if err != nil {
			return err
		}
		outputError = writeCreatedKey(output, created, options.json)
		logger.LogAttrs(ctx, slog.LevelInfo, "API key created", slog.String("event", "api_key_created"), slog.String("api_key_id", created.ID), slog.String("api_key_name", created.Name), slog.String("pairing_expires_at", *created.PairingExpiresAt))
	case "pair":
		paired, err := store.IssuePairing(ctx, options.id)
		if err != nil {
			return err
		}
		outputError = writePairing(output, paired, options.json)
		logger.LogAttrs(ctx, slog.LevelInfo, "Pairing code issued", slog.String("event", "pairing_code_issued"), slog.String("api_key_id", paired.ID), slog.String("pairing_expires_at", *paired.PairingExpiresAt))
	case "list":
		keys, err := store.List(ctx)
		if err != nil {
			return err
		}
		if !options.all {
			keys = slices.DeleteFunc(keys, func(key apikey.Key) bool { return key.RevokedAt != nil })
		}
		outputError = writeKeyList(output, keys, options.json)
		logger.LogAttrs(ctx, slog.LevelInfo, "API keys listed", slog.String("event", "api_keys_listed"), slog.Int("key_count", len(keys)))
	case "revoke":
		changed, err := store.Revoke(ctx, options.id)
		if err != nil {
			return err
		}
		outputError = writeRevocation(output, options.id, changed, options.json)
		logger.LogAttrs(ctx, slog.LevelInfo, "API key revoked", slog.String("event", "api_key_revoked"), slog.String("api_key_id", options.id), slog.Bool("changed", changed))
	}
	if outputError != nil {
		return oops.In("command").Code("command_output_failed").Wrapf(outputError, "command result could not be written")
	}
	logger.LogAttrs(ctx, slog.LevelDebug, "Local command completed", slog.String("event", "command_completed"), slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))))
	return nil
}
