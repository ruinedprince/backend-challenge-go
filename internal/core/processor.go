// Package core contains application use cases. It coordinates the domain
// aggregate with persistence but knows nothing about HTTP or SQS transports.
package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

var (
	ErrIdempotencyConflict        = errors.New("idempotency key conflicts with a different request")
	ErrExternalIdentityConflict   = errors.New("external transaction identity already exists with a different idempotency key")
	ErrUnsupportedTransactionType = errors.New("transaction type is not yet processable")
	ErrProviderWalletForbidden    = errors.New("provider cannot access this wallet")
)

type TransactionRequest struct {
	ProviderID            string
	ExternalTransactionID string
	IdempotencyKey        string
	PlayerID              string
	WalletID              string
	RoundID               string
	GameID                string
	Type                  domain.TransactionType
	Money                 domain.Money
	OccurredAt            time.Time
}

type TransactionResult struct {
	TransactionID    string
	Status           domain.TransactionState
	Balance          domain.Money
	IdempotentReplay bool
	FailureCode      string
}

type Processor struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewProcessor(pool *pgxpool.Pool) *Processor {
	return &Processor{pool: pool, now: time.Now}
}

// Process handles HTTP and SQS input identically. It locks exactly one wallet
// row, then writes the transaction, optional ledger entry, wallet version and
// outbox event inside the same SQL transaction.
func (p *Processor) Process(ctx context.Context, request TransactionRequest) (TransactionResult, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TransactionResult{}, fmt.Errorf("begin financial transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := p.processInTransaction(ctx, tx, request)
	if err != nil {
		return TransactionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TransactionResult{}, fmt.Errorf("commit financial transaction: %w", err)
	}
	return result, nil
}

func (p *Processor) processInTransaction(ctx context.Context, tx pgx.Tx, request TransactionRequest) (TransactionResult, error) {
	payloadHash, err := HashRequest(request)
	if err != nil {
		return TransactionResult{}, err
	}

	wallet, err := lockWallet(ctx, tx, request.WalletID)
	if err != nil {
		return TransactionResult{}, err
	}
	walletSnapshot := wallet.Snapshot()
	if walletSnapshot.PlayerID != request.PlayerID || walletSnapshot.Currency != request.Money.Currency() {
		return TransactionResult{}, fmt.Errorf("%w: request does not match wallet", domain.ErrInvalidTransaction)
	}
	if err := requireWalletAccess(ctx, tx, request.WalletID, request.ProviderID); err != nil {
		return TransactionResult{}, err
	}

	if previous, found, err := findByIdempotencyKey(ctx, tx, request.ProviderID, request.IdempotencyKey); err != nil {
		return TransactionResult{}, err
	} else if found {
		if previous.PayloadHash != payloadHash {
			return TransactionResult{}, ErrIdempotencyConflict
		}
		return previous.result(true)
	}
	if _, found, err := findByExternalIdentity(ctx, tx, request.ProviderID, request.ExternalTransactionID); err != nil {
		return TransactionResult{}, err
	} else if found {
		return TransactionResult{}, ErrExternalIdentityConflict
	}

	now := p.now().UTC()
	if request.OccurredAt.IsZero() {
		request.OccurredAt = now
	}
	transactionID, err := newUUID()
	if err != nil {
		return TransactionResult{}, err
	}
	domainTransaction, err := domain.NewExternalTransaction(domain.ExternalTransactionInput{
		ID: transactionID, ProviderID: request.ProviderID, ExternalTransactionID: request.ExternalTransactionID,
		IdempotencyKey: request.IdempotencyKey, PayloadHash: payloadHash, WalletID: request.WalletID,
		PlayerID: request.PlayerID, RoundID: request.RoundID, GameID: request.GameID,
		Type: request.Type, Money: request.Money, OccurredAt: request.OccurredAt,
	}, now)
	if err != nil {
		return TransactionResult{}, err
	}

	if err := insertPendingTransaction(ctx, tx, domainTransaction); err != nil {
		return TransactionResult{}, err
	}

	result, err := p.apply(ctx, tx, wallet, domainTransaction, now)
	if err != nil {
		return TransactionResult{}, err
	}
	return result, nil
}

func (p *Processor) apply(ctx context.Context, tx pgx.Tx, wallet *domain.Wallet, transaction *domain.WagerTransaction, now time.Time) (TransactionResult, error) {
	input := transaction.Input()
	var entry domain.WalletLedgerEntry
	var hasLedger bool
	var movementError error

	switch input.Type {
	case domain.TransactionBet:
		entry, movementError = wallet.Debit(input.ID, input.Money, now)
		hasLedger = movementError == nil
	case domain.TransactionWin:
		entry, movementError = wallet.Credit(input.ID, input.Money, now)
		hasLedger = movementError == nil
	case domain.TransactionLoss:
		// LOSS is a completed business fact but intentionally changes no balance.
	default:
		return TransactionResult{}, fmt.Errorf("%w: %s", ErrUnsupportedTransactionType, input.Type)
	}

	if movementError != nil {
		if !errors.Is(movementError, domain.ErrInsufficientFunds) {
			return TransactionResult{}, movementError
		}
		if err := transaction.TransitionTo(domain.TransactionRejected, now, "INSUFFICIENT_FUNDS"); err != nil {
			return TransactionResult{}, err
		}
		if err := finishTransaction(ctx, tx, transaction, wallet.Snapshot().Balance, wallet.Snapshot().Version); err != nil {
			return TransactionResult{}, err
		}
		if err := insertWagerOutboxEvent(ctx, tx, "WagerTransactionRejected", transaction, now); err != nil {
			return TransactionResult{}, err
		}
		return TransactionResult{TransactionID: input.ID, Status: transaction.State(), Balance: wallet.Snapshot().Balance, FailureCode: transaction.FailureCode()}, nil
	}

	if hasLedger {
		if err := updateWallet(ctx, tx, wallet); err != nil {
			return TransactionResult{}, err
		}
		if err := insertLedgerEntry(ctx, tx, entry); err != nil {
			return TransactionResult{}, err
		}
	}
	if err := transaction.TransitionTo(domain.TransactionProcessed, now, ""); err != nil {
		return TransactionResult{}, err
	}
	if err := finishTransaction(ctx, tx, transaction, wallet.Snapshot().Balance, wallet.Snapshot().Version); err != nil {
		return TransactionResult{}, err
	}
	if err := insertWagerOutboxEvent(ctx, tx, "WagerTransactionProcessed", transaction, now); err != nil {
		return TransactionResult{}, err
	}
	if hasLedger {
		if err := insertWalletBalanceChangedOutboxEvent(ctx, tx, entry, wallet.Snapshot().Version, input.ExternalTransactionID, now); err != nil {
			return TransactionResult{}, err
		}
	}
	return TransactionResult{TransactionID: input.ID, Status: transaction.State(), Balance: wallet.Snapshot().Balance}, nil
}

type persistedTransaction struct {
	ID          string
	PayloadHash string
	State       domain.TransactionState
	Balance     domain.Money
	FailureCode string
}

func (p persistedTransaction) result(replay bool) (TransactionResult, error) {
	return TransactionResult{TransactionID: p.ID, Status: p.State, Balance: p.Balance, IdempotentReplay: replay, FailureCode: p.FailureCode}, nil
}

func HashRequest(request TransactionRequest) (string, error) {
	// A struct has stable field ordering in encoding/json. The idempotency key
	// and transport metadata are deliberately absent from this canonical payload.
	payload := struct {
		ProviderID            string                 `json:"providerId"`
		ExternalTransactionID string                 `json:"externalTransactionId"`
		PlayerID              string                 `json:"playerId"`
		WalletID              string                 `json:"walletId"`
		RoundID               string                 `json:"roundId"`
		GameID                string                 `json:"gameId"`
		Type                  domain.TransactionType `json:"type"`
		Money                 domain.Money           `json:"money"`
	}{request.ProviderID, request.ExternalTransactionID, request.PlayerID, request.WalletID, request.RoundID, request.GameID, request.Type, request.Money}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal canonical transaction payload: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func newUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}
