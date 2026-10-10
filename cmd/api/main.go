package main

import (
	"log/slog"
	"os"

	"github.com/ruinedprince/backend-challenge-go/internal/app"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	application, err := app.New(logger)
	if err != nil {
		logger.Error("application setup failed", "error", err)
		os.Exit(1)
	}

	application.Run()
}
