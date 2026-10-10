package domain

import (
	"fmt"
	"strings"
	"time"
)

type TransactionType string

const (
	TransactionOpening  TransactionType = "OPENING"
	TransactionBet      TransactionType = "BET"
	TransactionWin      TransactionType = "WIN"
	TransactionLoss     TransactionType = "LOSS"
	TransactionRefund   TransactionType = "REFUND"
	TransactionRollback TransactionType = "ROLLBACK"
)

type TransactionState string

const (
	TransactionPending          TransactionState = "PENDING"
	TransactionPendingReference TransactionState = "PENDING_REFERENCE"
	TransactionProcessed        TransactionState = "PROCESSED"
	TransactionRejected         TransactionState = "REJECTED"
	TransactionFailed           TransactionState = "FAILED"
)

type ExternalTransactionInput struct {
	ID                             string
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Type                           TransactionType
	Money                          Money
	ReferenceExternalTransactionID string
	OccurredAt                     time.Time
}

// WagerTransaction owns the finite state machine for a requested financial
// operation. Persisting it and applying its wallet movement are later use-case
// responsibilities; this type prevents invalid transitions before I/O begins.
type WagerTransaction struct {
	input       ExternalTransactionInput
	state       TransactionState
	failureCode string
	createdAt   time.Time
	updatedAt   time.Time
}

func NewExternalTransaction(input ExternalTransactionInput, now time.Time) (*WagerTransaction, error) {
	if now.IsZero() || input.OccurredAt.IsZero() || anyBlank(input.ID, input.ProviderID, input.ExternalTransactionID, input.IdempotencyKey, input.PayloadHash, input.WalletID, input.PlayerID, input.RoundID, input.GameID) {
		return nil, fmt.Errorf("%w: required external fields are missing", ErrInvalidTransaction)
	}
	if input.Type == TransactionOpening {
		return nil, fmt.Errorf("%w: OPENING is internal only", ErrInvalidTransaction)
	}
	if err := input.Type.ValidateMoney(input.Money); err != nil {
		return nil, err
	}
	if (input.Type == TransactionRefund || input.Type == TransactionRollback) && strings.TrimSpace(input.ReferenceExternalTransactionID) == "" {
		return nil, fmt.Errorf("%w: a reference is required for %s", ErrInvalidTransaction, input.Type)
	}
	return &WagerTransaction{input: input, state: TransactionPending, createdAt: now.UTC(), updatedAt: now.UTC()}, nil
}

func (t TransactionType) ValidateMoney(money Money) error {
	switch t {
	case TransactionOpening:
		if money.IsNegative() {
			return fmt.Errorf("%w: OPENING cannot be negative", ErrInvalidTransaction)
		}
	case TransactionLoss:
		if !money.IsZero() {
			return fmt.Errorf("%w: LOSS must have amount 0.00", ErrInvalidTransaction)
		}
	case TransactionBet, TransactionWin, TransactionRefund, TransactionRollback:
		if !money.IsPositive() {
			return fmt.Errorf("%w: %s requires a positive amount", ErrInvalidTransaction, t)
		}
	default:
		return fmt.Errorf("%w: unknown type %q", ErrInvalidTransaction, t)
	}
	return nil
}

func (t *WagerTransaction) TransitionTo(next TransactionState, now time.Time, failureCode string) error {
	if now.IsZero() {
		return fmt.Errorf("%w: timestamp is required", ErrInvalidStateTransition)
	}
	if t.state.IsTerminal() {
		return fmt.Errorf("%w: %s is terminal", ErrInvalidStateTransition, t.state)
	}
	if !validTransition(t.state, next) {
		return fmt.Errorf("%w: %s to %s", ErrInvalidStateTransition, t.state, next)
	}
	if (next == TransactionRejected || next == TransactionFailed) && strings.TrimSpace(failureCode) == "" {
		return fmt.Errorf("%w: %s requires a failure code", ErrInvalidStateTransition, next)
	}
	if (next == TransactionPending || next == TransactionPendingReference || next == TransactionProcessed) && failureCode != "" {
		return fmt.Errorf("%w: %s cannot carry a failure code", ErrInvalidStateTransition, next)
	}
	t.state = next
	t.failureCode = failureCode
	t.updatedAt = now.UTC()
	return nil
}

func (s TransactionState) IsTerminal() bool {
	return s == TransactionProcessed || s == TransactionRejected || s == TransactionFailed
}

func (t *WagerTransaction) State() TransactionState         { return t.state }
func (t *WagerTransaction) FailureCode() string             { return t.failureCode }
func (t *WagerTransaction) Input() ExternalTransactionInput { return t.input }
func (t *WagerTransaction) CreatedAt() time.Time            { return t.createdAt }
func (t *WagerTransaction) UpdatedAt() time.Time            { return t.updatedAt }

func validTransition(current, next TransactionState) bool {
	switch current {
	case TransactionPending:
		return next == TransactionProcessed || next == TransactionPendingReference || next == TransactionRejected || next == TransactionFailed
	case TransactionPendingReference:
		return next == TransactionProcessed || next == TransactionRejected || next == TransactionFailed
	default:
		return false
	}
}

func anyBlank(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}
