package core

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

func TestProcessorConcurrentBetsAndReplay(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is required for PostgreSQL integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, "TRUNCATE outbox_events, inbox_messages, wallet_ledger_entries, wager_transactions, wallets CASCADE"); err != nil {
		t.Fatal(err)
	}
	const walletID = "00000000-0000-0000-0000-000000000001"
	const playerID = "00000000-0000-0000-0000-000000000011"
	if _, err := pool.Exec(ctx, `
        INSERT INTO wallets (id, player_id, currency, balance_minor_units)
        VALUES ($1, $2, 'BRL', 10000)
    `, walletID, playerID); err != nil {
		t.Fatal(err)
	}

	processor := NewProcessor(pool)
	amount, err := domain.ParseExternalMoney("80.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	requests := []TransactionRequest{
		{ProviderID: "provider-a", ExternalTransactionID: "bet-1", IdempotencyKey: "provider-a:bet-1", PlayerID: playerID, WalletID: walletID, RoundID: "round-1", GameID: "game-1", Type: domain.TransactionBet, Money: amount, OccurredAt: time.Now().UTC()},
		{ProviderID: "provider-a", ExternalTransactionID: "bet-2", IdempotencyKey: "provider-a:bet-2", PlayerID: playerID, WalletID: walletID, RoundID: "round-1", GameID: "game-1", Type: domain.TransactionBet, Money: amount, OccurredAt: time.Now().UTC()},
	}

	results := make(chan TransactionResult, 2)
	errorsFound := make(chan error, 2)
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, request := range requests {
		request := request
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, processErr := processor.Process(ctx, request)
			if processErr != nil {
				errorsFound <- processErr
				return
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsFound)

	var processed, rejected int
	var successful TransactionResult
	for result := range results {
		switch result.Status {
		case domain.TransactionProcessed:
			processed++
			successful = result
		case domain.TransactionRejected:
			rejected++
		default:
			t.Fatalf("unexpected result status %s", result.Status)
		}
	}
	for processErr := range errorsFound {
		t.Fatalf("Process() error = %v", processErr)
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed=%d rejected=%d, want 1 of each", processed, rejected)
	}

	var balance int64
	var ledgerCount int
	if err := pool.QueryRow(ctx, "SELECT balance_minor_units FROM wallets WHERE id = $1", walletID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1", walletID).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if balance != 2000 || ledgerCount != 1 {
		t.Fatalf("balance=%d ledger=%d, want 2000 and 1", balance, ledgerCount)
	}

	var successfulExternalID string
	if err := pool.QueryRow(ctx, "SELECT external_transaction_id FROM wager_transactions WHERE state = 'PROCESSED'").Scan(&successfulExternalID); err != nil {
		t.Fatal(err)
	}
	replayRequest := requests[0]
	if requests[1].ExternalTransactionID == successfulExternalID {
		replayRequest = requests[1]
	}
	if successful.TransactionID == "" {
		t.Fatal("successful transaction id is empty")
	}
	replay, err := processor.Process(ctx, replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.IdempotentReplay || replay.Status != domain.TransactionProcessed || replay.Balance.String() != "20.00" {
		t.Fatalf("replay = %+v, want persisted processed result", replay)
	}

	changedPayload := replayRequest
	changedPayload.Money, err = domain.ParseExternalMoney("70.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	_, err = processor.Process(ctx, changedPayload)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed payload error = %v, want ErrIdempotencyConflict", err)
	}
}
