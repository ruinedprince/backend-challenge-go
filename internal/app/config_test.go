package app

import "testing"

func TestLoadConfigAcceptsLocalEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://challenge:challenge@postgres:5432/challenge?sslmode=disable")
	t.Setenv("KEYCLOAK_ISSUER_URL", "http://keycloak:8080/realms/wagering")
	t.Setenv("SQS_ENDPOINT_URL", "http://localstack:4566")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("APP_SHUTDOWN_TIMEOUT", "10s")

	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.HTTPAddr != ":8080" {
		t.Fatalf("HTTPAddr = %q, want :8080", config.HTTPAddr)
	}
}

func TestLoadConfigRejectsInvalidSQSURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://challenge:challenge@postgres:5432/challenge?sslmode=disable")
	t.Setenv("KEYCLOAK_ISSUER_URL", "http://keycloak:8080/realms/wagering")
	t.Setenv("SQS_ENDPOINT_URL", "not a URL")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() error = nil, want invalid SQS endpoint error")
	}
}
