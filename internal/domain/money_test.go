package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParseMoneyNormalizesToTwoDecimalPlaces(t *testing.T) {
	t.Parallel()

	money, err := ParseMoney("25.5", "BRL")
	if err != nil {
		t.Fatalf("ParseMoney() error = %v", err)
	}
	if got, want := money.String(), "25.50"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := money.MinorUnits(), int64(2550); got != want {
		t.Fatalf("MinorUnits() = %d, want %d", got, want)
	}
}

func TestParseExternalMoneyRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		amount   string
		currency string
		want     error
	}{
		{name: "empty", amount: "", currency: "BRL", want: ErrInvalidAmount},
		{name: "negative", amount: "-1.00", currency: "BRL", want: ErrNegativeExternalAmount},
		{name: "excess scale", amount: "1.001", currency: "BRL", want: ErrInvalidAmount},
		{name: "scientific notation", amount: "1e2", currency: "BRL", want: ErrInvalidAmount},
		{name: "not a number", amount: "NaN", currency: "BRL", want: ErrInvalidAmount},
		{name: "invalid currency", amount: "1.00", currency: "brl", want: ErrInvalidCurrency},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseExternalMoney(testCase.amount, testCase.currency)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("ParseExternalMoney(%q, %q) error = %v, want %v", testCase.amount, testCase.currency, err, testCase.want)
			}
		})
	}
}

func TestMoneyArithmeticRejectsCurrencyMismatchAndOverflow(t *testing.T) {
	t.Parallel()

	brl := mustMoney(t, "10.00", "BRL")
	usd := mustMoney(t, "10.00", "USD")
	if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add() error = %v, want ErrCurrencyMismatch", err)
	}

	maximum, err := NewMoney(math.MaxInt64, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	one, err := NewMoney(1, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maximum.Add(one); !errors.Is(err, ErrMoneyOverflow) {
		t.Fatalf("Add() error = %v, want ErrMoneyOverflow", err)
	}
}

func TestParseMoneyHandlesInt64Boundary(t *testing.T) {
	t.Parallel()

	minimum, err := ParseMoney("-92233720368547758.08", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := minimum.MinorUnits(), int64(math.MinInt64); got != want {
		t.Fatalf("MinorUnits() = %d, want %d", got, want)
	}
	if _, err := ParseMoney("92233720368547758.08", "BRL"); !errors.Is(err, ErrMoneyOverflow) {
		t.Fatalf("positive overflow error = %v, want ErrMoneyOverflow", err)
	}
}

func TestMoneyJSONRoundTripUsesDecimalStrings(t *testing.T) {
	t.Parallel()

	original := mustMoney(t, "25.00", "BRL")
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"amount":"25.00","currency":"BRL"}`; got != want {
		t.Fatalf("MarshalJSON() = %s, want %s", got, want)
	}

	var decoded Money
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if comparison, err := original.Compare(decoded); err != nil || comparison != 0 {
		t.Fatalf("decoded money differs: comparison=%d, error=%v", comparison, err)
	}
}

func mustMoney(t *testing.T, amount, currency string) Money {
	t.Helper()
	money, err := ParseMoney(amount, currency)
	if err != nil {
		t.Fatalf("ParseMoney(%q, %q): %v", amount, currency, err)
	}
	return money
}
