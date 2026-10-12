package core

import (
	"errors"
	"testing"
	"time"
)

func TestLedgerCursorRoundTripAndRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	want := ledgerCursor{CreatedAt: time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC), EntryID: "entry-1"}
	encoded, err := encodeLedgerCursor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeLedgerCursor(encoded)
	if err != nil || *got != want {
		t.Fatalf("decoded cursor = %+v, %v; want %+v", got, err, want)
	}
	if _, err := decodeLedgerCursor("not-a-cursor"); !errors.Is(err, ErrInvalidLedgerCursor) {
		t.Fatalf("invalid cursor error = %v, want ErrInvalidLedgerCursor", err)
	}
}
