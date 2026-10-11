package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ceclin/notify-qq/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.RunCommand(ctx, os.Args[1:], os.Stdout); err != nil {
		os.Exit(1)
	}
}
