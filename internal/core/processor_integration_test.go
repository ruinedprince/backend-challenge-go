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
	if _, err := pool.Exec(ctx, `INSERT INTO wallet_provider_access (wallet_id, provider_id) VALUES ($1, 'provider-a')`, walletID); err != nil {
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

func TestProcessorResolvesPendingReferenceAndExpiresMissingReference(t *testing.T) {
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
	const walletID = "00000000-0000-0000-0000-000000000101"
	const playerID = "00000000-0000-0000-0000-000000000111"
	if _, err := pool.Exec(ctx, `INSERT INTO wallets (id, player_id, currency, balance_minor_units) VALUES ($1,$2,'BRL',10000)`, walletID, playerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO wallet_provider_access (wallet_id, provider_id) VALUES ($1,'provider-a')`, walletID); err != nil {
		t.Fatal(err)
	}
	processor := NewProcessor(pool)
	money, err := domain.ParseExternalMoney("25.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	request := func(externalID string, kind domain.TransactionType, reference string) TransactionRequest {
		return TransactionRequest{ProviderID: "provider-a", ExternalTransactionID: externalID, IdempotencyKey: "provider-a:" + externalID, PlayerID: playerID, WalletID: walletID, RoundID: "round-reference", GameID: "game-reference", Type: kind, Money: money, ReferenceExternalTransactionID: reference, OccurredAt: time.Now().UTC()}
	}
	pending, err := processor.Process(ctx, request("refund-before-bet", domain.TransactionRefund, "bet-original"))
	if err != nil || pending.Status != domain.TransactionPendingReference {
		t.Fatalf("pending refund = %+v, %v", pending, err)
	}
	if _, err = processor.Process(ctx, request("bet-original", domain.TransactionBet, "")); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE wager_transactions SET reference_next_attempt_at=now()-interval '1 second' WHERE id=$1`, pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	if err = processor.ResolvePendingReferences(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var state, failureCode string
	var balance int64
	if err = pool.QueryRow(ctx, `SELECT state,COALESCE(failure_code,''),result_balance_minor_units FROM wager_transactions WHERE id=$1`, pending.TransactionID).Scan(&state, &failureCode, &balance); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.TransactionProcessed) || failureCode != "" || balance != 10000 {
		t.Fatalf("resolved refund state=%s code=%s balance=%d", state, failureCode, balance)
	}
	rollback, err := processor.Process(ctx, request("rollback-refund", domain.TransactionRollback, "refund-before-bet"))
	if err != nil || rollback.Status != domain.TransactionProcessed || rollback.Balance.String() != "75.00" {
		t.Fatalf("rollback of refund = %+v, %v", rollback, err)
	}
	duplicate, err := processor.Process(ctx, request("duplicate-refund", domain.TransactionRefund, "bet-original"))
	if err != nil || duplicate.Status != domain.TransactionRejected || duplicate.FailureCode != "REVERSAL_ALREADY_APPLIED" {
		t.Fatalf("duplicate reversal = %+v, %v", duplicate, err)
	}

	expiring, err := processor.Process(ctx, request("refund-never-found", domain.TransactionRefund, "never-arrives"))
	if err != nil || expiring.Status != domain.TransactionPendingReference {
		t.Fatalf("expiring refund = %+v, %v", expiring, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE wager_transactions SET reference_attempts=5,reference_next_attempt_at=now()-interval '1 second' WHERE id=$1`, expiring.TransactionID); err != nil {
		t.Fatal(err)
	}
	if err = processor.ResolvePendingReferences(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT state,COALESCE(failure_code,'') FROM wager_transactions WHERE id=$1`, expiring.TransactionID).Scan(&state, &failureCode); err != nil {
		t.Fatal(err)
	}
	if state != string(domain.TransactionRejected) || failureCode != "REFERENCE_NOT_FOUND" {
		t.Fatalf("expired reference state=%s code=%s", state, failureCode)
	}
}

func TestProcessorFiftyConcurrentReplaysApplyOneMovement(t *testing.T) {
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
	const walletID = "00000000-0000-0000-0000-000000000201"
	const playerID = "00000000-0000-0000-0000-000000000211"
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
	request := TransactionRequest{ProviderID: "provider-a", ExternalTransactionID: "replayed-bet", IdempotencyKey: "provider-a:replayed-bet", PlayerID: playerID, WalletID: walletID, RoundID: "round-replay", GameID: "game-replay", Type: domain.TransactionBet, Money: money, OccurredAt: time.Now().UTC()}
	processor := NewProcessor(pool)
	start := make(chan struct{})
	results := make(chan TransactionResult, 50)
	errorsFound := make(chan error, 50)
	var group sync.WaitGroup
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := processor.Process(ctx, request)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("replay error: %v", err)
	}
	var firstDelivery, replays int
	for result := range results {
		if result.Status != domain.TransactionProcessed || result.Balance.String() != "75.00" {
			t.Fatalf("unexpected replay result: %+v", result)
		}
		if result.IdempotentReplay {
			replays++
		} else {
			firstDelivery++
		}
	}
	if firstDelivery != 1 || replays != 49 {
		t.Fatalf("first=%d replays=%d, want 1/49", firstDelivery, replays)
	}
	var balance int64
	var ledgers int
	if err := pool.QueryRow(ctx, `SELECT balance_minor_units FROM wallets WHERE id=$1`, walletID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&ledgers); err != nil {
		t.Fatal(err)
	}
	if balance != 7500 || ledgers != 1 {
		t.Fatalf("balance=%d ledgers=%d, want 7500/1", balance, ledgers)
	}
}

func TestProcessorInboxRedeliveryIsDurableNoOp(t *testing.T) {
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
	const walletID = "00000000-0000-0000-0000-000000000301"
	const playerID = "00000000-0000-0000-0000-000000000311"
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
	request := TransactionRequest{ProviderID: "provider-a", ExternalTransactionID: "redelivered-bet", IdempotencyKey: "provider-a:redelivered-bet", PlayerID: playerID, WalletID: walletID, RoundID: "round-inbox", GameID: "game-inbox", Type: domain.TransactionBet, Money: money, OccurredAt: time.Now().UTC()}
	processor := NewProcessor(pool)
	first, duplicate, err := processor.ProcessInbox(ctx, "wager-transactions", "message-redelivered", "same-payload-hash", request)
	if err != nil || duplicate || first.Status != domain.TransactionProcessed {
		t.Fatalf("first inbox processing = %+v duplicate=%t err=%v", first, duplicate, err)
	}
	_, duplicate, err = processor.ProcessInbox(ctx, "wager-transactions", "message-redelivered", "same-payload-hash", request)
	if err != nil || !duplicate {
		t.Fatalf("redelivery duplicate=%t err=%v", duplicate, err)
	}
	var balance int64
	var ledgers int
	if err := pool.QueryRow(ctx, `SELECT balance_minor_units FROM wallets WHERE id=$1`, walletID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&ledgers); err != nil {
		t.Fatal(err)
	}
	if balance != 7500 || ledgers != 1 {
		t.Fatalf("redelivery changed balance=%d ledgers=%d", balance, ledgers)
	}
}
