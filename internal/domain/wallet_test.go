package domain

import (
	"errors"
	"testing"
	"time"
)

func TestWalletDebitCreatesMatchingLedgerEntryAndIncrementsVersion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	wallet, err := NewWallet("wallet-1", "player-1", mustMoney(t, "100.00", "BRL"), now)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := wallet.Debit("transaction-1", mustMoney(t, "25.00", "BRL"), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := wallet.Snapshot()
	if got, want := snapshot.Balance.String(), "75.00"; got != want {
		t.Fatalf("balance = %s, want %s", got, want)
	}
	if got, want := snapshot.Version, int64(2); got != want {
		t.Fatalf("version = %d, want %d", got, want)
	}
	if got, want := entry.Direction(), LedgerDebit; got != want {
		t.Fatalf("direction = %s, want %s", got, want)
	}
	if got, want := entry.BalanceAfter().String(), "75.00"; got != want {
		t.Fatalf("ledger balance after = %s, want %s", got, want)
	}
}

func TestWalletRejectsInvalidMovementWithoutChangingState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	wallet, err := NewWallet("wallet-1", "player-1", mustMoney(t, "10.00", "BRL"), now)
	if err != nil {
		t.Fatal(err)
	}

	_, err = wallet.Debit("transaction-1", mustMoney(t, "11.00", "BRL"), now)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("Debit() error = %v, want ErrInsufficientFunds", err)
	}
	_, err = wallet.Credit("transaction-2", mustMoney(t, "1.00", "USD"), now)
	if !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Credit() error = %v, want ErrCurrencyMismatch", err)
	}
	snapshot := wallet.Snapshot()
	if got, want := snapshot.Balance.String(), "10.00"; got != want {
		t.Fatalf("balance changed after rejection: got %s, want %s", got, want)
	}
	if got, want := snapshot.Version, int64(1); got != want {
		t.Fatalf("version changed after rejection: got %d, want %d", got, want)
	}
}

func TestWalletLedgerEntryRejectsIncorrectBalanceTransition(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	_, err := NewWalletLedgerEntry(
		"wallet-1", "transaction-1", LedgerDebit,
		mustMoney(t, "10.00", "BRL"),
		mustMoney(t, "100.00", "BRL"),
		mustMoney(t, "95.00", "BRL"),
		now,
	)
	if !errors.Is(err, ErrInvalidLedgerEntry) {
		t.Fatalf("NewWalletLedgerEntry() error = %v, want ErrInvalidLedgerEntry", err)
	}
}
