// Package domain contains the business rules that do not depend on HTTP,
// PostgreSQL, SQS, or the dependency-injection framework.
package domain

import "errors"

var (
	ErrInvalidAmount          = errors.New("invalid monetary amount")
	ErrInvalidCurrency        = errors.New("invalid currency")
	ErrCurrencyMismatch       = errors.New("currency mismatch")
	ErrMoneyOverflow          = errors.New("monetary amount overflow")
	ErrNegativeExternalAmount = errors.New("negative external monetary amount")
	ErrInvalidWallet          = errors.New("invalid wallet")
	ErrInsufficientFunds      = errors.New("insufficient wallet balance")
	ErrInvalidLedgerEntry     = errors.New("invalid wallet ledger entry")
	ErrInvalidTransaction     = errors.New("invalid wager transaction")
	ErrInvalidStateTransition = errors.New("invalid transaction state transition")
)
