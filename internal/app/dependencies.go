package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/ruinedprince/backend-challenge-go/internal/auth"
	"github.com/ruinedprince/backend-challenge-go/internal/core"
)

// Module is the framework boundary. Domain packages will not import Fx.
var Module = fx.Module("application",
	fx.Provide(LoadConfig),
	fx.Provide(NewPostgresPool),
	fx.Provide(NewSQSClient),
	fx.Provide(NewReadiness),
	fx.Provide(NewMetrics),
	fx.Provide(core.NewProcessor),
	fx.Provide(newOIDCVerifier),
	fx.Provide(NewAPI),
	fx.Provide(NewSQSConsumer),
	fx.Provide(NewOutboxPublisher),
	fx.Provide(NewReferenceResolver),
	fx.Provide(NewHTTPServer),
	fx.Invoke(RegisterDependencies),
	fx.Invoke(RegisterSQSConsumer),
	fx.Invoke(RegisterOutboxPublisher),
	fx.Invoke(RegisterReferenceResolver),
	// Fx stops lifecycle hooks in reverse order. Registering HTTP last makes it
	// the first component to stop accepting new requests during shutdown.
	fx.Invoke(RegisterHTTPServer),
)

func newOIDCVerifier(config Config) *auth.Verifier {
	return auth.NewVerifier(config.KeycloakIssuer, config.InternalClientID)
}

func NewPostgresPool(config Config) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	return pgxpool.NewWithConfig(context.Background(), poolConfig)
}

func NewSQSClient(config Config) (*sqs.Client, error) {
	awsConfig, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(config.AWSRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(config.AWSAccessKeyID, config.AWSSecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}

	return sqs.NewFromConfig(awsConfig, func(options *sqs.Options) {
		options.BaseEndpoint = aws.String(config.SQSEndpointURL)
	}), nil
}

type Readiness struct {
	pool      *pgxpool.Pool
	sqsClient *sqs.Client
}

func NewReadiness(pool *pgxpool.Pool, sqsClient *sqs.Client) *Readiness {
	return &Readiness{pool: pool, sqsClient: sqsClient}
}

func RegisterDependencies(lifecycle fx.Lifecycle, pool *pgxpool.Pool, readiness *Readiness) {
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			return readiness.Check(ctx)
		},
		OnStop: func(context.Context) error {
			pool.Close()
			return nil
		},
	})
}

func (r *Readiness) Check(ctx context.Context) error {
	if err := r.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}
	if _, err := r.sqsClient.ListQueues(ctx, &sqs.ListQueuesInput{MaxResults: aws.Int32(1)}); err != nil {
		return fmt.Errorf("list SQS queues: %w", err)
	}
	return nil
}

func (r *Readiness) Handler(writer http.ResponseWriter, request *http.Request) {
	if err := r.Check(request.Context()); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}
