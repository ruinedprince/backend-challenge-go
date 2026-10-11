package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

// eventPayload is the immutable, business-level snapshot stored in the outbox.
// Transport concerns (SQS URL, retry count and publication time) stay outside it.
type eventPayload struct {
	CorrelationID string `json:"correlationId"`
	Data          any    `json:"data"`
}

type wagerEventData struct {
	TransactionID string                  `json:"transactionId"`
	ProviderID    string                  `json:"providerId"`
	WalletID      string                  `json:"walletId"`
	PlayerID      string                  `json:"playerId"`
	RoundID       string                  `json:"roundId"`
	GameID        string                  `json:"gameId"`
	Type          domain.TransactionType  `json:"type"`
	State         domain.TransactionState `json:"state"`
	Money         domain.Money            `json:"money"`
	FailureCode   string                  `json:"failureCode,omitempty"`
}

type walletBalanceChangedData struct {
	WalletID      string                 `json:"walletId"`
	TransactionID string                 `json:"transactionId"`
	Direction     domain.LedgerDirection `json:"direction"`
	Money         domain.Money           `json:"money"`
	BalanceBefore domain.Money           `json:"balanceBefore"`
	BalanceAfter  domain.Money           `json:"balanceAfter"`
	WalletVersion int64                  `json:"walletVersion"`
}

func insertWagerOutboxEvent(ctx context.Context, tx pgx.Tx, eventType string, transaction *domain.WagerTransaction, occurredAt time.Time) error {
	input := transaction.Input()
	payload, err := json.Marshal(eventPayload{
		CorrelationID: input.ExternalTransactionID,
		Data:          wagerEventData{TransactionID: input.ID, ProviderID: input.ProviderID, WalletID: input.WalletID, PlayerID: input.PlayerID, RoundID: input.RoundID, GameID: input.GameID, Type: input.Type, State: transaction.State(), Money: input.Money, FailureCode: transaction.FailureCode()},
	})
	if err != nil {
		return fmt.Errorf("marshal wager outbox payload: %w", err)
	}
	return insertOutboxRecord(ctx, tx, input.ID, eventType, payload, occurredAt)
}

func insertWalletBalanceChangedOutboxEvent(ctx context.Context, tx pgx.Tx, entry domain.WalletLedgerEntry, walletVersion int64, correlationID string, occurredAt time.Time) error {
	payload, err := json.Marshal(eventPayload{
		CorrelationID: correlationID,
		Data:          walletBalanceChangedData{WalletID: entry.WalletID(), TransactionID: entry.TransactionID(), Direction: entry.Direction(), Money: entry.Money(), BalanceBefore: entry.BalanceBefore(), BalanceAfter: entry.BalanceAfter(), WalletVersion: walletVersion},
	})
	if err != nil {
		return fmt.Errorf("marshal balance-change outbox payload: %w", err)
	}
	return insertOutboxRecord(ctx, tx, entry.TransactionID(), "WalletBalanceChanged", payload, occurredAt)
}

func insertOutboxRecord(ctx context.Context, tx pgx.Tx, aggregateID, eventType string, payload []byte, occurredAt time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events (aggregate_id,event_type,payload,occurred_at) VALUES ($1,$2,$3,$4)`, aggregateID, eventType, payload, occurredAt.UTC()); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}
