package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/ceclin/notify-qq/internal/logging"
)

func RunCommand(ctx context.Context, arguments []string, output io.Writer) error {
	commandID := ulid.Make().String()
	logger := slog.New(logging.NewLogger(os.Stderr, slog.LevelInfo).Handler().WithAttrs([]slog.Attr{slog.String("command_id", commandID)}))
	ctx = logging.WithLogger(ctx, logger)
	started := time.Now()
	var showVersion, jsonOutput, serving bool
	root := &cobra.Command{
		Use:           "notify-qq",
		Short:         "Send notifications to QQ private chats and groups",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		RunE: func(command *cobra.Command, _ []string) error {
			if showVersion {
				return writeVersion(command.OutOrStdout(), jsonOutput)
			}
			return command.Help()
		},
	}
	root.Flags().BoolVar(&showVersion, "version", false, "Show version and build metadata")
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output results as JSON")
	root.AddCommand(
		&cobra.Command{
			Use:   "serve",
			Short: "Start the HTTP service and QQ WebSocket worker",
			Args:  cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				// Run owns reporting of service lifecycle failures.
				serving = true
				return Run(command.Context())
			},
		},
		&cobra.Command{
			Use:   "healthcheck",
			Short: "Check local HTTP readiness",
			Args:  cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				return healthcheck(command.Context())
			},
		},
		newKeysCommand(commandID, &jsonOutput),
	)
	// An empty list must not fall back to Cobra's process arguments.
	root.SetArgs(append([]string{}, arguments...))
	root.SetOut(output)
	root.SetErr(os.Stderr)
	command, err := root.ExecuteContextC(ctx)
	if err != nil && !serving {
		ctx = command.Context()
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "Local command failed", slog.String("event", "command_failed"), slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))), logging.ErrorAttribute(err))
	}
	return err
}
