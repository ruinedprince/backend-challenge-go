package domain

import (
	"fmt"
	"strings"
	"time"
)

type LedgerDirection string

const (
	LedgerDebit  LedgerDirection = "DEBIT"
	LedgerCredit LedgerDirection = "CREDIT"
)

// Wallet is the financial aggregate. Its balance can only change through
// Debit and Credit, which return the ledger entry that must be persisted in
// the same SQL transaction as the updated wallet.
type Wallet struct {
	id        string
	playerID  string
	currency  string
	balance   Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

type WalletSnapshot struct {
	ID        string
	PlayerID  string
	Currency  string
	Balance   Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func NewWallet(id, playerID string, initialBalance Money, now time.Time) (*Wallet, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(playerID) == "" || now.IsZero() {
		return nil, fmt.Errorf("%w: id, player id, and timestamps are required", ErrInvalidWallet)
	}
	if initialBalance.IsNegative() {
		return nil, fmt.Errorf("%w: initial balance cannot be negative", ErrInvalidWallet)
	}
	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  initialBalance.Currency(),
		balance:   initialBalance,
		version:   1,
		createdAt: now.UTC(),
		updatedAt: now.UTC(),
	}, nil
}

// RehydrateWallet restores a previously validated wallet without applying a
// financial movement. Rehydration must never increment version or emit ledger.
func RehydrateWallet(snapshot WalletSnapshot) (*Wallet, error) {
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.PlayerID) == "" ||
		snapshot.CreatedAt.IsZero() || snapshot.UpdatedAt.IsZero() || snapshot.Version < 1 {
		return nil, fmt.Errorf("%w: invalid persisted wallet", ErrInvalidWallet)
	}
	if snapshot.Balance.IsNegative() || snapshot.Currency != snapshot.Balance.Currency() {
		return nil, fmt.Errorf("%w: invalid persisted balance", ErrInvalidWallet)
	}
	return &Wallet{
		id:        snapshot.ID,
		playerID:  snapshot.PlayerID,
		currency:  snapshot.Currency,
		balance:   snapshot.Balance,
		version:   snapshot.Version,
		createdAt: snapshot.CreatedAt.UTC(),
		updatedAt: snapshot.UpdatedAt.UTC(),
	}, nil
}

func (w *Wallet) Snapshot() WalletSnapshot {
	return WalletSnapshot{
		ID: w.id, PlayerID: w.playerID, Currency: w.currency, Balance: w.balance,
		Version: w.version, CreatedAt: w.createdAt, UpdatedAt: w.updatedAt,
	}
}

func (w *Wallet) Debit(transactionID string, amount Money, now time.Time) (WalletLedgerEntry, error) {
	if err := w.validateMovement(transactionID, amount, now); err != nil {
		return WalletLedgerEntry{}, err
	}
	comparison, err := w.balance.Compare(amount)
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	if comparison < 0 {
		return WalletLedgerEntry{}, fmt.Errorf("%w: balance %s, requested %s", ErrInsufficientFunds, w.balance.String(), amount.String())
	}
	after, err := w.balance.Subtract(amount)
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	entry, err := NewWalletLedgerEntry(w.id, transactionID, LedgerDebit, amount, w.balance, after, now)
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	w.applyBalance(after, now)
	return entry, nil
}

func (w *Wallet) Credit(transactionID string, amount Money, now time.Time) (WalletLedgerEntry, error) {
	if err := w.validateMovement(transactionID, amount, now); err != nil {
		return WalletLedgerEntry{}, err
	}
	after, err := w.balance.Add(amount)
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	entry, err := NewWalletLedgerEntry(w.id, transactionID, LedgerCredit, amount, w.balance, after, now)
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	w.applyBalance(after, now)
	return entry, nil
}

func (w *Wallet) validateMovement(transactionID string, amount Money, now time.Time) error {
	if strings.TrimSpace(transactionID) == "" || now.IsZero() {
		return fmt.Errorf("%w: transaction id and timestamp are required", ErrInvalidWallet)
	}
	if amount.Currency() != w.currency {
		return fmt.Errorf("%w: wallet %s, amount %s", ErrCurrencyMismatch, w.currency, amount.Currency())
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: movement must be positive", ErrInvalidAmount)
	}
	return nil
}

func (w *Wallet) applyBalance(balance Money, now time.Time) {
	w.balance = balance
	w.version++
	w.updatedAt = now.UTC()
}

// WalletLedgerEntry is immutable proof of one applied balance movement.
type WalletLedgerEntry struct {
	walletID      string
	transactionID string
	direction     LedgerDirection
	money         Money
	balanceBefore Money
	balanceAfter  Money
	createdAt     time.Time
}

func NewWalletLedgerEntry(walletID, transactionID string, direction LedgerDirection, money, balanceBefore, balanceAfter Money, createdAt time.Time) (WalletLedgerEntry, error) {
	if strings.TrimSpace(walletID) == "" || strings.TrimSpace(transactionID) == "" || createdAt.IsZero() || !money.IsPositive() {
		return WalletLedgerEntry{}, fmt.Errorf("%w: required fields are missing or amount is not positive", ErrInvalidLedgerEntry)
	}
	if direction != LedgerDebit && direction != LedgerCredit {
		return WalletLedgerEntry{}, fmt.Errorf("%w: unknown direction %q", ErrInvalidLedgerEntry, direction)
	}
	var expected Money
	var err error
	if direction == LedgerDebit {
		expected, err = balanceBefore.Subtract(money)
	} else {
		expected, err = balanceBefore.Add(money)
	}
	if err != nil {
		return WalletLedgerEntry{}, fmt.Errorf("%w: %v", ErrInvalidLedgerEntry, err)
	}
	if expected != balanceAfter || balanceAfter.IsNegative() {
		return WalletLedgerEntry{}, fmt.Errorf("%w: balance transition does not match direction and amount", ErrInvalidLedgerEntry)
	}
	return WalletLedgerEntry{
		walletID: walletID, transactionID: transactionID, direction: direction,
		money: money, balanceBefore: balanceBefore, balanceAfter: balanceAfter, createdAt: createdAt.UTC(),
	}, nil
}

func (e WalletLedgerEntry) WalletID() string           { return e.walletID }
func (e WalletLedgerEntry) TransactionID() string      { return e.transactionID }
func (e WalletLedgerEntry) Direction() LedgerDirection { return e.direction }
func (e WalletLedgerEntry) Money() Money               { return e.money }
func (e WalletLedgerEntry) BalanceBefore() Money       { return e.balanceBefore }
func (e WalletLedgerEntry) BalanceAfter() Money        { return e.balanceAfter }
func (e WalletLedgerEntry) CreatedAt() time.Time       { return e.createdAt }
