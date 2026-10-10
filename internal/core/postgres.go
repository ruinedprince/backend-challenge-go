package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

func lockWallet(ctx context.Context, tx pgx.Tx, walletID string) (*domain.Wallet, error) {
	var snapshot domain.WalletSnapshot
	var balanceMinorUnits int64
	err := tx.QueryRow(ctx, `
        SELECT id, player_id, currency, balance_minor_units, version, created_at, updated_at
        FROM wallets
        WHERE id = $1
        FOR UPDATE
    `, walletID).Scan(&snapshot.ID, &snapshot.PlayerID, &snapshot.Currency, &balanceMinorUnits, &snapshot.Version, &snapshot.CreatedAt, &snapshot.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("%w: wallet %s", domain.ErrInvalidWallet, walletID)
		}
		return nil, fmt.Errorf("lock wallet: %w", err)
	}
	balance, err := domain.NewMoney(balanceMinorUnits, snapshot.Currency)
	if err != nil {
		return nil, err
	}
	snapshot.Balance = balance
	return domain.RehydrateWallet(snapshot)
}

func findByIdempotencyKey(ctx context.Context, tx pgx.Tx, providerID, key string) (persistedTransaction, bool, error) {
	return findPersistedTransaction(ctx, tx, `
        SELECT id, payload_hash, state, result_balance_minor_units, currency, failure_code
        FROM wager_transactions
        WHERE provider_id = $1 AND idempotency_key = $2
    `, providerID, key)
}

func findByExternalIdentity(ctx context.Context, tx pgx.Tx, providerID, externalID string) (persistedTransaction, bool, error) {
	return findPersistedTransaction(ctx, tx, `
        SELECT id, payload_hash, state, result_balance_minor_units, currency, failure_code
        FROM wager_transactions
        WHERE provider_id = $1 AND external_transaction_id = $2
    `, providerID, externalID)
}

func findPersistedTransaction(ctx context.Context, tx pgx.Tx, query, providerID, value string) (persistedTransaction, bool, error) {
	var persisted persistedTransaction
	var balanceMinorUnits *int64
	var currency string
	var failureCode *string
	err := tx.QueryRow(ctx, query, providerID, value).Scan(
		&persisted.ID, &persisted.PayloadHash, &persisted.State, &balanceMinorUnits, &currency, &failureCode,
	)
	if err == pgx.ErrNoRows {
		return persistedTransaction{}, false, nil
	}
	if err != nil {
		return persistedTransaction{}, false, fmt.Errorf("find persisted transaction: %w", err)
	}
	if balanceMinorUnits == nil {
		return persistedTransaction{}, false, fmt.Errorf("persisted transaction %s has no result balance", persisted.ID)
	}
	balance, err := domain.NewMoney(*balanceMinorUnits, currency)
	if err != nil {
		return persistedTransaction{}, false, err
	}
	persisted.Balance = balance
	if failureCode != nil {
		persisted.FailureCode = *failureCode
	}
	return persisted, true, nil
}

func insertPendingTransaction(ctx context.Context, tx pgx.Tx, transaction *domain.WagerTransaction) error {
	input := transaction.Input()
	_, err := tx.Exec(ctx, `
        INSERT INTO wager_transactions (
            id, provider_id, external_transaction_id, idempotency_key, payload_hash,
            wallet_id, player_id, round_id, game_id, transaction_type, amount_minor_units,
            currency, reference_external_transaction_id, state, occurred_at, created_at, updated_at
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULL, $13, $14, $15, $15)
    `, input.ID, input.ProviderID, input.ExternalTransactionID, input.IdempotencyKey, input.PayloadHash,
		input.WalletID, input.PlayerID, input.RoundID, input.GameID, input.Type, input.Money.MinorUnits(),
		input.Money.Currency(), transaction.State(), input.OccurredAt, transaction.CreatedAt())
	if err != nil {
		return fmt.Errorf("insert pending transaction: %w", err)
	}
	return nil
}

func updateWallet(ctx context.Context, tx pgx.Tx, wallet *domain.Wallet) error {
	snapshot := wallet.Snapshot()
	command, err := tx.Exec(ctx, `
        UPDATE wallets
        SET balance_minor_units = $1, version = $2, updated_at = $3
        WHERE id = $4 AND version = $5
    `, snapshot.Balance.MinorUnits(), snapshot.Version, snapshot.UpdatedAt, snapshot.ID, snapshot.Version-1)
	if err != nil {
		return fmt.Errorf("update wallet balance: %w", err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("update wallet balance: concurrent wallet update")
	}
	return nil
}

func insertLedgerEntry(ctx context.Context, tx pgx.Tx, entry domain.WalletLedgerEntry) error {
	_, err := tx.Exec(ctx, `
        INSERT INTO wallet_ledger_entries (
            wallet_id, transaction_id, direction, amount_minor_units, currency,
            balance_before_minor_units, balance_after_minor_units, created_at
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
    `, entry.WalletID(), entry.TransactionID(), entry.Direction(), entry.Money().MinorUnits(), entry.Money().Currency(),
		entry.BalanceBefore().MinorUnits(), entry.BalanceAfter().MinorUnits(), entry.CreatedAt())
	if err != nil {
		return fmt.Errorf("insert wallet ledger entry: %w", err)
	}
	return nil
}

func finishTransaction(ctx context.Context, tx pgx.Tx, transaction *domain.WagerTransaction, balance domain.Money, walletVersion int64) error {
	_, err := tx.Exec(ctx, `
        UPDATE wager_transactions
        SET state = $1, failure_code = $2, result_balance_minor_units = $3,
            result_wallet_version = $4, updated_at = $5
        WHERE id = $6
    `, transaction.State(), nullableFailureCode(transaction.FailureCode()), balance.MinorUnits(), walletVersion, transaction.UpdatedAt(), transaction.Input().ID)
	if err != nil {
		return fmt.Errorf("finish transaction: %w", err)
	}
	return nil
}

func insertOutboxEvent(ctx context.Context, tx pgx.Tx, aggregateID, eventType string, input domain.ExternalTransactionInput, occurredAt time.Time) error {
	payload, err := json.Marshal(struct {
		TransactionID string                 `json:"transactionId"`
		ProviderID    string                 `json:"providerId"`
		WalletID      string                 `json:"walletId"`
		Type          domain.TransactionType `json:"type"`
	}{aggregateID, input.ProviderID, input.WalletID, input.Type})
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO outbox_events (aggregate_id, event_type, payload, occurred_at)
        VALUES ($1, $2, $3, $4)
    `, aggregateID, eventType, payload, occurredAt)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

func nullableFailureCode(failureCode string) any {
	if failureCode == "" {
		return nil
	}
	return failureCode
}
