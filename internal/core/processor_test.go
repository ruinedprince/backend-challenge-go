package core

import (
	"testing"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

func TestHashRequestIgnoresIdempotencyKeyAndUsesCanonicalMoney(t *testing.T) {
	t.Parallel()
	money, err := domain.ParseExternalMoney("25.0", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	first := TransactionRequest{
		ProviderID: "provider-a", ExternalTransactionID: "external-1", IdempotencyKey: "one",
		PlayerID: "player-1", WalletID: "wallet-1", RoundID: "round-1", GameID: "game-1",
		Type: domain.TransactionBet, Money: money,
	}
	second := first
	second.IdempotencyKey = "two"

	firstHash, err := HashRequest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := HashRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash {
		t.Fatalf("idempotency key changed payload hash: %s != %s", firstHash, secondHash)
	}
}

func TestNewUUIDProducesUUIDv4Shape(t *testing.T) {
	t.Parallel()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '4' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("newUUID() = %q, not a UUID v4", id)
	}
}
