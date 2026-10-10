package domain

import (
	"errors"
	"testing"
	"time"
)

func TestTransactionTypesValidateTheirMoneyRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		type_  TransactionType
		amount string
		valid  bool
	}{
		{name: "bet positive", type_: TransactionBet, amount: "1.00", valid: true},
		{name: "win positive", type_: TransactionWin, amount: "1.00", valid: true},
		{name: "loss zero", type_: TransactionLoss, amount: "0.00", valid: true},
		{name: "loss nonzero", type_: TransactionLoss, amount: "1.00", valid: false},
		{name: "refund zero", type_: TransactionRefund, amount: "0.00", valid: false},
		{name: "rollback positive", type_: TransactionRollback, amount: "1.00", valid: true},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.type_.ValidateMoney(mustMoney(t, testCase.amount, "BRL"))
			if testCase.valid && err != nil {
				t.Fatalf("ValidateMoney() error = %v", err)
			}
			if !testCase.valid && !errors.Is(err, ErrInvalidTransaction) {
				t.Fatalf("ValidateMoney() error = %v, want ErrInvalidTransaction", err)
			}
		})
	}
}

func TestExternalTransactionStateMachine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	transaction, err := NewExternalTransaction(validExternalInput(t, TransactionBet, "25.00"), now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := transaction.State(), TransactionPending; got != want {
		t.Fatalf("initial state = %s, want %s", got, want)
	}
	if err := transaction.TransitionTo(TransactionPendingReference, now, ""); err != nil {
		t.Fatal(err)
	}
	if err := transaction.TransitionTo(TransactionProcessed, now, ""); err != nil {
		t.Fatal(err)
	}
	if err := transaction.TransitionTo(TransactionRejected, now, "RULE"); !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("terminal transition error = %v, want ErrInvalidStateTransition", err)
	}
}

func TestExternalTransactionRejectsOpeningAndMissingReversalReference(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	_, err := NewExternalTransaction(validExternalInput(t, TransactionOpening, "1.00"), now)
	if !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("OPENING error = %v, want ErrInvalidTransaction", err)
	}
	input := validExternalInput(t, TransactionRefund, "1.00")
	_, err = NewExternalTransaction(input, now)
	if !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("missing reference error = %v, want ErrInvalidTransaction", err)
	}
}

func validExternalInput(t *testing.T, transactionType TransactionType, amount string) ExternalTransactionInput {
	t.Helper()
	return ExternalTransactionInput{
		ID: "transaction-1", ProviderID: "provider-a", ExternalTransactionID: "external-1",
		IdempotencyKey: "provider-a:external-1", PayloadHash: "hash", WalletID: "wallet-1",
		PlayerID: "player-1", RoundID: "round-1", GameID: "game-1", Type: transactionType,
		Money: mustMoney(t, amount, "BRL"), OccurredAt: time.Now().UTC(),
	}
}
