package app

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOutboxPublisherThreeInstancesClaimWithoutDuplicates(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("SQS_ENDPOINT_URL") == "" {
		t.Skip("DATABASE_URL and SQS_ENDPOINT_URL are required for integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "TRUNCATE outbox_events"); err != nil {
		t.Fatal(err)
	}
	config := Config{AWSRegion: os.Getenv("AWS_REGION"), AWSAccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), AWSSecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SQSEndpointURL: os.Getenv("SQS_ENDPOINT_URL")}
	client, err := NewSQSClient(config)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(outboxQueueName)})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"20000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000002", "20000000-0000-4000-8000-000000000003"} {
		staleClaim := id == "20000000-0000-4000-8000-000000000003"
		if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (id,aggregate_id,event_type,payload,occurred_at,claimed_at) VALUES ($1,'21111111-1111-4111-8111-111111111111','WagerTransactionProcessed','{"correlationId":"outbox-test","data":{"transactionId":"test"}}',now(),CASE WHEN $2 THEN now()-interval '31 seconds' ELSE NULL END)`, id, staleClaim); err != nil {
			t.Fatal(err)
		}
	}
	var group sync.WaitGroup
	errorsFound := make(chan error, 3)
	for range 3 {
		group.Add(1)
		go func() {
			defer group.Done()
			publisher := NewOutboxPublisher(pool, client, slog.Default())
			publisher.queueURL = aws.ToString(queue.QueueUrl)
			if err := publisher.publishAvailable(ctx); err != nil {
				errorsFound <- err
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	var published, attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE published_at IS NOT NULL),max(attempts) FROM outbox_events`).Scan(&published, &attempts); err != nil {
		t.Fatal(err)
	}
	if published != 3 || attempts != 1 {
		t.Fatalf("published=%d max attempts=%d, want 3/1", published, attempts)
	}
}
