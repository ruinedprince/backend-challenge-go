package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

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
			publisher := NewOutboxPublisher(pool, client, slog.Default(), NewMetrics(pool))
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

func TestOutboxPublisherRecoversCommittedEventAfterRestart(t *testing.T) {
	ctx, pool, client, queueURL := outboxIntegrationDependencies(t)
	eventID := newOutboxTestID()
	insertOutboxEvent(t, ctx, pool, eventID, newOutboxTestID())

	// This represents a process dying after the financial transaction committed
	// its outbox row but before any publisher got a chance to run.
	publisher := NewOutboxPublisher(pool, client, slog.Default(), NewMetrics(pool))
	publisher.queueURL = queueURL
	if err := publisher.publishAvailable(ctx); err != nil {
		t.Fatal(err)
	}

	assertOutboxPublished(t, ctx, pool, eventID, 1)
	assertEventMessage(t, ctx, client, queueURL, eventID)
}

func TestOutboxPublisherRecoversAfterSendBeforeConfirmation(t *testing.T) {
	ctx, pool, client, queueURL := outboxIntegrationDependencies(t)
	eventID := newOutboxTestID()
	insertOutboxEvent(t, ctx, pool, eventID, newOutboxTestID())

	crashCtx, stopPublisher := context.WithCancel(ctx)
	afterSendReached := make(chan struct{})
	firstPublisher := NewOutboxPublisher(pool, client, slog.Default(), NewMetrics(pool))
	firstPublisher.queueURL = queueURL
	firstPublisher.afterSend = func(ctx context.Context, _ outboxEvent) error {
		close(afterSendReached)
		<-ctx.Done()
		return ctx.Err()
	}
	publishResult := make(chan error, 1)
	go func() { publishResult <- firstPublisher.publishAvailable(crashCtx) }()
	<-afterSendReached
	stopPublisher()
	if err := <-publishResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted publish error = %v, want context cancellation", err)
	}

	var publishedAt *string
	var claimedAt *string
	if err := pool.QueryRow(ctx, `SELECT published_at::text, claimed_at::text FROM outbox_events WHERE id=$1`, eventID).Scan(&publishedAt, &claimedAt); err != nil {
		t.Fatal(err)
	}
	if publishedAt != nil || claimedAt == nil {
		t.Fatalf("after simulated crash published_at=%v claimed_at=%v, want pending leased event", publishedAt, claimedAt)
	}

	// A second process takes over only after the dead publisher's lease expires.
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET claimed_at=now()-interval '31 seconds' WHERE id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	secondPublisher := NewOutboxPublisher(pool, client, slog.Default(), NewMetrics(pool))
	secondPublisher.queueURL = queueURL
	if err := secondPublisher.publishAvailable(ctx); err != nil {
		t.Fatal(err)
	}

	assertOutboxPublished(t, ctx, pool, eventID, 2)
	assertEventMessage(t, ctx, client, queueURL, eventID)
}

func outboxIntegrationDependencies(t *testing.T) (context.Context, *pgxpool.Pool, *sqs.Client, string) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("SQS_ENDPOINT_URL") == "" {
		t.Skip("DATABASE_URL and SQS_ENDPOINT_URL are required for integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "TRUNCATE outbox_events"); err != nil {
		t.Fatal(err)
	}
	config := Config{AWSRegion: os.Getenv("AWS_REGION"), AWSAccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), AWSSecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SQSEndpointURL: os.Getenv("SQS_ENDPOINT_URL")}
	client, err := NewSQSClient(config)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(fmt.Sprintf("outbox-recovery-%x.fifo", time.Now().UnixNano())),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	queueURL := aws.ToString(queue.QueueUrl)
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)})
	})
	return ctx, pool, client, queueURL
}

func insertOutboxEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID, aggregateID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (id,aggregate_id,event_type,payload,occurred_at) VALUES ($1,$2,'WagerTransactionProcessed','{"correlationId":"outbox-recovery","data":{"transactionId":"test"}}',now())`, eventID, aggregateID); err != nil {
		t.Fatal(err)
	}
}

func newOutboxTestID() string {
	return fmt.Sprintf("20000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
}

func assertOutboxPublished(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID string, wantAttempts int) {
	t.Helper()
	var published bool
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT published_at IS NOT NULL,attempts FROM outbox_events WHERE id=$1`, eventID).Scan(&published, &attempts); err != nil {
		t.Fatal(err)
	}
	if !published || attempts != wantAttempts {
		t.Fatalf("published=%t attempts=%d, want true/%d", published, attempts, wantAttempts)
	}
}

func assertEventMessage(t *testing.T, ctx context.Context, client *sqs.Client, queueURL, wantEventID string) {
	t.Helper()
	result, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range result.Messages {
		var event struct {
			EventID string `json:"eventId"`
		}
		if err := json.Unmarshal([]byte(aws.ToString(message.Body)), &event); err != nil {
			t.Fatal(err)
		}
		if event.EventID == wantEventID {
			if _, err := client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: message.ReceiptHandle}); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("event %s was not published to wager-events.fifo", wantEventID)
}
