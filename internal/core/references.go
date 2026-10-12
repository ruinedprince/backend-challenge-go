package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

const maxReferenceAttempts = 5

type reversalReference struct {
	ID       string
	Provider string
	WalletID string
	PlayerID string
	RoundID  string
	Type     domain.TransactionType
	State    domain.TransactionState
	Money    domain.Money
}

// ResolvePendingReferences is safe to run in every application instance.
// SKIP LOCKED grants each durable pending record to at most one resolver.
func (p *Processor) ResolvePendingReferences(ctx context.Context, limit int) error {
	if limit < 1 {
		limit = 1
	}
	rows, err := p.pool.Query(ctx, `
        SELECT id FROM wager_transactions
        WHERE state='PENDING_REFERENCE' AND reference_next_attempt_at <= now()
        ORDER BY reference_next_attempt_at, created_at
        FOR UPDATE SKIP LOCKED LIMIT $1
    `, limit)
	if err != nil {
		return fmt.Errorf("find pending references: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, id := range ids {
		if err := p.resolvePendingReference(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) resolvePendingReference(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var input domain.ExternalTransactionInput
	var state domain.TransactionState
	var attempts int
	var createdAt, updatedAt time.Time
	var amount int64
	var currency string
	err = tx.QueryRow(ctx, `
        SELECT provider_id,external_transaction_id,idempotency_key,payload_hash,wallet_id,player_id,round_id,game_id,
               transaction_type,amount_minor_units,currency,reference_external_transaction_id,state,reference_attempts,occurred_at,created_at,updated_at
        FROM wager_transactions
        WHERE id=$1 AND state='PENDING_REFERENCE' AND reference_next_attempt_at <= now()
        FOR UPDATE
    `, id).Scan(&input.ProviderID, &input.ExternalTransactionID, &input.IdempotencyKey, &input.PayloadHash, &input.WalletID, &input.PlayerID, &input.RoundID, &input.GameID,
		&input.Type, &amount, &currency, &input.ReferenceExternalTransactionID, &state, &attempts, &input.OccurredAt, &createdAt, &updatedAt)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load pending reference: %w", err)
	}
	input.ID = id
	input.Money, err = domain.NewMoney(amount, currency)
	if err != nil {
		return err
	}
	transaction, err := domain.RehydrateExternalTransaction(input, state, "", createdAt, updatedAt)
	if err != nil {
		return err
	}
	wallet, err := lockWallet(ctx, tx, input.WalletID)
	if err != nil {
		return err
	}
	if _, err = p.apply(ctx, tx, wallet, transaction, p.now().UTC(), attempts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Processor) applyReversal(ctx context.Context, tx pgx.Tx, wallet *domain.Wallet, transaction *domain.WagerTransaction, now time.Time, previousAttempts int) (TransactionResult, error) {
	input := transaction.Input()
	reference, found, err := findTransactionReference(ctx, tx, input.ProviderID, input.ReferenceExternalTransactionID)
	if err != nil {
		return TransactionResult{}, err
	}
	if !found || reference.State == domain.TransactionPending || reference.State == domain.TransactionPendingReference {
		return p.deferReference(ctx, tx, wallet, transaction, now, previousAttempts)
	}
	if reference.State != domain.TransactionProcessed {
		return p.rejectTransaction(ctx, tx, wallet, transaction, now, "REFERENCE_NOT_PROCESSABLE", nil)
	}
	if failureCode := validateReference(input, reference); failureCode != "" {
		return p.rejectTransaction(ctx, tx, wallet, transaction, now, failureCode, &reference.ID)
	}
	if reference.Type == domain.TransactionBet {
		var alreadyApplied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wager_transactions WHERE provider_id=$1 AND reference_external_transaction_id=$2 AND transaction_type IN ('REFUND','ROLLBACK') AND state='PROCESSED')`, input.ProviderID, input.ReferenceExternalTransactionID).Scan(&alreadyApplied); err != nil {
			return TransactionResult{}, err
		}
		if alreadyApplied {
			return p.rejectTransaction(ctx, tx, wallet, transaction, now, "REVERSAL_ALREADY_APPLIED", &reference.ID)
		}
	}

	var entry domain.WalletLedgerEntry
	if reversalDirection(input.Type, reference.Type) == domain.LedgerCredit {
		entry, err = wallet.Credit(input.ID, input.Money, now)
	} else {
		entry, err = wallet.Debit(input.ID, input.Money, now)
	}
	if err != nil {
		if errors.Is(err, domain.ErrInsufficientFunds) {
			return p.rejectTransaction(ctx, tx, wallet, transaction, now, "INSUFFICIENT_FUNDS_FOR_REVERSAL", &reference.ID)
		}
		return TransactionResult{}, err
	}
	if err = updateWallet(ctx, tx, wallet); err != nil {
		return TransactionResult{}, err
	}
	if err = insertLedgerEntry(ctx, tx, entry); err != nil {
		return TransactionResult{}, err
	}
	if err = transaction.TransitionTo(domain.TransactionProcessed, now, ""); err != nil {
		return TransactionResult{}, err
	}
	if err = finishTransaction(ctx, tx, transaction, wallet.Snapshot().Balance, wallet.Snapshot().Version, &reference.ID); err != nil {
		return TransactionResult{}, err
	}
	if err = insertWagerOutboxEvent(ctx, tx, "WagerTransactionProcessed", transaction, now); err != nil {
		return TransactionResult{}, err
	}
	if err = insertWalletBalanceChangedOutboxEvent(ctx, tx, entry, wallet.Snapshot().Version, input.ExternalTransactionID, now); err != nil {
		return TransactionResult{}, err
	}
	return TransactionResult{TransactionID: input.ID, Status: transaction.State(), Balance: wallet.Snapshot().Balance}, nil
}

func (p *Processor) deferReference(ctx context.Context, tx pgx.Tx, wallet *domain.Wallet, transaction *domain.WagerTransaction, now time.Time, previousAttempts int) (TransactionResult, error) {
	if previousAttempts >= maxReferenceAttempts {
		return p.rejectTransaction(ctx, tx, wallet, transaction, now, "REFERENCE_NOT_FOUND", nil)
	}
	firstAttempt := transaction.State() == domain.TransactionPending
	if firstAttempt {
		if err := transaction.TransitionTo(domain.TransactionPendingReference, now, ""); err != nil {
			return TransactionResult{}, err
		}
	}
	attempts := previousAttempts + 1
	_, err := tx.Exec(ctx, `UPDATE wager_transactions SET state=$1,result_balance_minor_units=$2,result_wallet_version=$3,reference_attempts=$4,reference_next_attempt_at=$5,updated_at=$6 WHERE id=$7`, transaction.State(), wallet.Snapshot().Balance.MinorUnits(), wallet.Snapshot().Version, attempts, now.Add(referenceBackoff(attempts)), now, transaction.Input().ID)
	if err != nil {
		return TransactionResult{}, fmt.Errorf("defer reference: %w", err)
	}
	if firstAttempt {
		if err := insertWagerOutboxEvent(ctx, tx, "WagerTransactionPendingReference", transaction, now); err != nil {
			return TransactionResult{}, err
		}
	}
	return TransactionResult{TransactionID: transaction.Input().ID, Status: transaction.State(), Balance: wallet.Snapshot().Balance}, nil
}

func (p *Processor) rejectTransaction(ctx context.Context, tx pgx.Tx, wallet *domain.Wallet, transaction *domain.WagerTransaction, now time.Time, failureCode string, referenceID *string) (TransactionResult, error) {
	if err := transaction.TransitionTo(domain.TransactionRejected, now, failureCode); err != nil {
		return TransactionResult{}, err
	}
	if err := finishTransaction(ctx, tx, transaction, wallet.Snapshot().Balance, wallet.Snapshot().Version, referenceID); err != nil {
		return TransactionResult{}, err
	}
	if err := insertWagerOutboxEvent(ctx, tx, "WagerTransactionRejected", transaction, now); err != nil {
		return TransactionResult{}, err
	}
	return TransactionResult{TransactionID: transaction.Input().ID, Status: transaction.State(), Balance: wallet.Snapshot().Balance, FailureCode: failureCode}, nil
}

// resolveWinReference verifies an optional WIN-to-BET link. Unlike a reversal,
// a WIN is never held pending: its reference is supporting evidence, not a
// prerequisite that can later change the direction or amount of the movement.
func (p *Processor) resolveWinReference(ctx context.Context, tx pgx.Tx, input domain.ExternalTransactionInput) (*string, string, error) {
	reference, found, err := findTransactionReference(ctx, tx, input.ProviderID, input.ReferenceExternalTransactionID)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "WIN_REFERENCE_NOT_FOUND", nil
	}
	if reference.State != domain.TransactionProcessed {
		return &reference.ID, "WIN_REFERENCE_NOT_PROCESSABLE", nil
	}
	if input.ProviderID != reference.Provider || input.WalletID != reference.WalletID || input.PlayerID != reference.PlayerID || input.RoundID != reference.RoundID || input.Money.Currency() != reference.Money.Currency() {
		return &reference.ID, "WIN_REFERENCE_MISMATCH", nil
	}
	if reference.Type != domain.TransactionBet {
		return &reference.ID, "WIN_REFERENCE_TYPE_INVALID", nil
	}
	return &reference.ID, "", nil
}

func findTransactionReference(ctx context.Context, tx pgx.Tx, providerID, externalID string) (reversalReference, bool, error) {
	var reference reversalReference
	var amount int64
	var currency string
	err := tx.QueryRow(ctx, `SELECT id,provider_id,wallet_id,player_id,round_id,transaction_type,state,amount_minor_units,currency FROM wager_transactions WHERE provider_id=$1 AND external_transaction_id=$2 FOR UPDATE`, providerID, externalID).Scan(&reference.ID, &reference.Provider, &reference.WalletID, &reference.PlayerID, &reference.RoundID, &reference.Type, &reference.State, &amount, &currency)
	if err == pgx.ErrNoRows {
		return reversalReference{}, false, nil
	}
	if err != nil {
		return reversalReference{}, false, err
	}
	var moneyErr error
	reference.Money, moneyErr = domain.NewMoney(amount, currency)
	return reference, moneyErr == nil, moneyErr
}

func validateReference(input domain.ExternalTransactionInput, reference reversalReference) string {
	if input.ProviderID != reference.Provider || input.WalletID != reference.WalletID || input.PlayerID != reference.PlayerID || input.RoundID != reference.RoundID || input.Money != reference.Money {
		return "REFERENCE_MISMATCH"
	}
	if input.Type == domain.TransactionRefund && reference.Type != domain.TransactionBet {
		return "REFERENCE_TYPE_INVALID"
	}
	if input.Type == domain.TransactionRollback && reference.Type != domain.TransactionBet && reference.Type != domain.TransactionWin && reference.Type != domain.TransactionRefund {
		return "REFERENCE_TYPE_INVALID"
	}
	return ""
}

func reversalDirection(kind, referenceKind domain.TransactionType) domain.LedgerDirection {
	if kind == domain.TransactionRefund || referenceKind == domain.TransactionBet {
		return domain.LedgerCredit
	}
	return domain.LedgerDebit
}

func referenceBackoff(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<attempt) * time.Second
}
