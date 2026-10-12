package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ruinedprince/backend-challenge-go/internal/core"
	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

func TestSQSConsumerRedeliveryAfterCommitDoesNotDuplicateDebit(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("SQS_ENDPOINT_URL") == "" {
		t.Skip("DATABASE_URL and SQS_ENDPOINT_URL are required for SQS integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "TRUNCATE outbox_events, inbox_messages, wallet_ledger_entries, wager_transactions, wallets CASCADE"); err != nil {
		t.Fatal(err)
	}

	client, err := NewSQSClient(Config{AWSRegion: os.Getenv("AWS_REGION"), AWSAccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), AWSSecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SQSEndpointURL: os.Getenv("SQS_ENDPOINT_URL")})
	if err != nil {
		t.Fatal(err)
	}
	queueName := fmt.Sprintf("consumer-redelivery-%d.fifo", time.Now().UnixNano())
	queue, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queueName), Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false", "VisibilityTimeout": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	queueURL := aws.ToString(queue.QueueUrl)
	defer func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)})
	}()

	const walletID = "00000000-0000-0000-0000-000000000401"
	const playerID = "00000000-0000-0000-0000-000000000411"
	if _, err := pool.Exec(ctx, `INSERT INTO wallets (id,player_id,currency,balance_minor_units) VALUES ($1,$2,'BRL',10000)`, walletID, playerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO wallet_provider_access (wallet_id,provider_id) VALUES ($1,'provider-a')`, walletID); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(map[string]any{
		"messageId": "commit-before-delete-message",
		"type":      "WagerTransaction",
		"data": map[string]any{
			"providerId": "provider-a", "externalTransactionId": "commit-before-delete-bet",
			"idempotencyKey": "provider-a:commit-before-delete-bet", "playerId": playerID,
			"walletId": walletID, "roundId": "round-redelivery", "gameId": "game-redelivery",
			"kind": "BET", "money": map[string]string{"amount": "25.00", "currency": "BRL"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queueURL), MessageBody: aws.String(string(body)), MessageGroupId: aws.String("redelivery-wallet"), MessageDeduplicationId: aws.String("commit-before-delete-1")}); err != nil {
		t.Fatal(err)
	}

	processor := core.NewProcessor(pool)
	committed := make(chan struct{})
	first := NewSQSConsumer(client, processor, slog.Default())
	first.queueName = queueName
	first.visibilityTimeout = 1
	first.afterCommit = func(ctx context.Context, _ sqsTypesMessage) {
		close(committed)
		<-ctx.Done() // Simulates an abrupt process stop before DeleteMessage.
	}
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, committed, 5*time.Second, "first consumer to commit the transaction")
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := first.Stop(stopCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()

	redelivered := make(chan struct{})
	deleted := make(chan struct{})
	second := NewSQSConsumer(client, processor, slog.Default())
	second.queueName = queueName
	second.visibilityTimeout = 1
	second.afterCommit = func(context.Context, sqsTypesMessage) { close(redelivered) }
	second.afterDelete = func(context.Context, sqsTypesMessage) { close(deleted) }
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, redelivered, 5*time.Second, "second consumer to receive the redelivery")
	waitForSignal(t, deleted, 5*time.Second, "second consumer to delete the redelivery")
	secondStopCtx, secondCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer secondCancel()
	if err := second.Stop(secondStopCtx); err != nil {
		t.Fatal(err)
	}

	var balance int64
	var ledgerEntries, inboxEntries int
	if err := pool.QueryRow(ctx, `SELECT balance_minor_units FROM wallets WHERE id=$1`, walletID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&ledgerEntries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE consumer_name='wager-transactions' AND message_id='commit-before-delete-message'`).Scan(&inboxEntries); err != nil {
		t.Fatal(err)
	}
	if balance != 7500 || ledgerEntries != 1 || inboxEntries != 1 {
		t.Fatalf("balance=%d ledgerEntries=%d inboxEntries=%d, want 7500/1/1", balance, ledgerEntries, inboxEntries)
	}
}

func TestSQSConsumerAndHTTPPathShareFinancialIdempotency(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("SQS_ENDPOINT_URL") == "" {
		t.Skip("DATABASE_URL and SQS_ENDPOINT_URL are required for SQS integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "TRUNCATE outbox_events, inbox_messages, wallet_ledger_entries, wager_transactions, wallets CASCADE"); err != nil {
		t.Fatal(err)
	}
	client, err := NewSQSClient(Config{AWSRegion: os.Getenv("AWS_REGION"), AWSAccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), AWSSecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SQSEndpointURL: os.Getenv("SQS_ENDPOINT_URL")})
	if err != nil {
		t.Fatal(err)
	}
	queueName := fmt.Sprintf("consumer-http-sqs-%d.fifo", time.Now().UnixNano())
	queue, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(queueName), Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	queueURL := aws.ToString(queue.QueueUrl)
	defer func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)})
	}()

	const walletID = "00000000-0000-0000-0000-000000000421"
	const playerID = "00000000-0000-0000-0000-000000000431"
	if _, err := pool.Exec(ctx, `INSERT INTO wallets (id,player_id,currency,balance_minor_units) VALUES ($1,$2,'BRL',10000)`, walletID, playerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO wallet_provider_access (wallet_id,provider_id) VALUES ($1,'provider-a')`, walletID); err != nil {
		t.Fatal(err)
	}
	money, err := domain.ParseExternalMoney("25.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	request := core.TransactionRequest{ProviderID: "provider-a", ExternalTransactionID: "http-sqs-same-bet", IdempotencyKey: "provider-a:http-sqs-same-bet", PlayerID: playerID, WalletID: walletID, RoundID: "round-http-sqs", GameID: "game-http-sqs", Type: domain.TransactionBet, Money: money, OccurredAt: time.Now().UTC()}
	processor := core.NewProcessor(pool)
	body, err := json.Marshal(map[string]any{
		"messageId": "http-sqs-same-message", "type": "WagerTransactionRequested",
		"data": map[string]any{"providerId": request.ProviderID, "externalTransactionId": request.ExternalTransactionID, "idempotencyKey": request.IdempotencyKey, "playerId": playerID, "walletId": walletID, "roundId": request.RoundID, "gameId": request.GameID, "kind": "BET", "money": map[string]string{"amount": "25.00", "currency": "BRL"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queueURL), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(walletID), MessageDeduplicationId: aws.String("http-sqs-same-message")}); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan struct{})
	sqsReady := make(chan struct{})
	start := make(chan struct{})
	httpResult := make(chan core.TransactionResult, 1)
	httpErrors := make(chan error, 1)
	consumer := NewSQSConsumer(client, processor, slog.Default())
	consumer.queueName = queueName
	consumer.beforeProcess = func(context.Context, sqsTypesMessage) {
		close(sqsReady)
		<-start
	}
	consumer.afterDelete = func(context.Context, sqsTypesMessage) { close(deleted) }
	if err := consumer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, sqsReady, 5*time.Second, "SQS consumer to be ready for the same operation")
	go func() {
		<-start
		result, err := processor.Process(ctx, request) // application path used by HTTP
		if err != nil {
			httpErrors <- err
			return
		}
		httpResult <- result
	}()
	close(start) // releases the HTTP and SQS paths at the same time.
	select {
	case err := <-httpErrors:
		t.Fatalf("HTTP path error: %v", err)
	case result := <-httpResult:
		if result.Status != domain.TransactionProcessed || result.Balance.String() != "75.00" {
			t.Fatalf("HTTP path result = %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP path")
	}
	waitForSignal(t, deleted, 5*time.Second, "SQS duplicate to be acknowledged")
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := consumer.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	var balance int64
	var ledgerEntries, inboxEntries int
	if err := pool.QueryRow(ctx, `SELECT balance_minor_units FROM wallets WHERE id=$1`, walletID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&ledgerEntries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE consumer_name='wager-transactions' AND message_id='http-sqs-same-message'`).Scan(&inboxEntries); err != nil {
		t.Fatal(err)
	}
	if balance != 7500 || ledgerEntries != 1 || inboxEntries != 1 {
		t.Fatalf("balance=%d ledgerEntries=%d inboxEntries=%d, want 7500/1/1", balance, ledgerEntries, inboxEntries)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, timeout time.Duration, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}
