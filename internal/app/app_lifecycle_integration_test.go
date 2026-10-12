package app

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestApplicationFxLifecycleStartsAndStopsWorkers(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("SQS_ENDPOINT_URL") == "" || os.Getenv("KEYCLOAK_ISSUER_URL") == "" {
		t.Skip("real application dependencies are required for lifecycle integration test")
	}
	application, err := New(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := application.fxApp.Start(ctx); err != nil {
		t.Fatalf("start application: %v", err)
	}
	if err := application.fxApp.Stop(ctx); err != nil {
		t.Fatalf("stop application: %v", err)
	}
}
