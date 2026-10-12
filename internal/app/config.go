package app

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr         string
	ShutdownTimeout  time.Duration
	DatabaseURL      string
	KeycloakIssuer   string
	InternalClientID string
	SQSEndpointURL   string
	AWSRegion        string
	AWSAccessKeyID   string
	AWSSecretKey     string
	OTLPEndpoint     string
}

func LoadConfig() (Config, error) {
	shutdownTimeout, err := time.ParseDuration(valueOrDefault("APP_SHUTDOWN_TIMEOUT", "10s"))
	if err != nil {
		return Config{}, fmt.Errorf("parse APP_SHUTDOWN_TIMEOUT: %w", err)
	}

	config := Config{
		HTTPAddr:         valueOrDefault("APP_HTTP_ADDR", ":8080"),
		ShutdownTimeout:  shutdownTimeout,
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		KeycloakIssuer:   os.Getenv("KEYCLOAK_ISSUER_URL"),
		InternalClientID: valueOrDefault("INTERNAL_SERVICE_CLIENT_ID", "wallet-service"),
		SQSEndpointURL:   os.Getenv("SQS_ENDPOINT_URL"),
		AWSRegion:        os.Getenv("AWS_REGION"),
		AWSAccessKeyID:   os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretKey:     os.Getenv("AWS_SECRET_ACCESS_KEY"),
		OTLPEndpoint:     valueOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", "http://jaeger:4318"),
	}

	if config.DatabaseURL == "" || config.KeycloakIssuer == "" || config.InternalClientID == "" || config.SQSEndpointURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL, KEYCLOAK_ISSUER_URL and SQS_ENDPOINT_URL are required")
	}
	if shutdownTimeout <= 0 {
		return Config{}, fmt.Errorf("APP_SHUTDOWN_TIMEOUT must be positive")
	}
	if err := requireHTTPURL("KEYCLOAK_ISSUER_URL", config.KeycloakIssuer); err != nil {
		return Config{}, err
	}
	if err := requireHTTPURL("SQS_ENDPOINT_URL", config.SQSEndpointURL); err != nil {
		return Config{}, err
	}
	if config.AWSRegion == "" || config.AWSAccessKeyID == "" || config.AWSSecretKey == "" {
		return Config{}, fmt.Errorf("AWS_REGION, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}

	return config, nil
}

func requireHTTPURL(name, rawURL string) error {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute HTTP URL", name)
	}
	if strings.ContainsAny(parsed.Host, " ") {
		return fmt.Errorf("%s contains an invalid host", name)
	}
	return nil
}

func valueOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
