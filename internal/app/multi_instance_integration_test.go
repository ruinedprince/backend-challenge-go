package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestThreeAPIInstancesShareIdempotencyAndLedger(t *testing.T) {
	urls := strings.Split(os.Getenv("MULTI_INSTANCE_API_URLS"), ",")
	if len(urls) != 3 || urls[0] == "" || os.Getenv("KEYCLOAK_ISSUER_URL") == "" {
		t.Skip("three API URLs and Keycloak are required")
	}

	providerToken := keycloakClientToken(t, "provider-a", "provider-a-secret")
	internalToken := keycloakClientToken(t, "wallet-service", "wallet-service-secret")
	internalHeaders := map[string]string{"Authorization": "Bearer " + internalToken}
	wallet := createMultiInstanceWallet(t, urls[0], internalHeaders)

	requestBody, err := json.Marshal(map[string]any{
		"providerId": "provider-a", "externalTransactionId": "three-api-replay-" + uniqueTestID(),
		"playerId": wallet.PlayerID, "walletId": wallet.ID, "roundId": "three-api-round", "gameId": "three-api-game",
		"kind": "BET", "money": map[string]string{"amount": "25.00", "currency": "BRL"},
	})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		Status           string `json:"status"`
		IdempotentReplay bool   `json:"idempotentReplay"`
		Err              error
	}
	results := make(chan result, 50)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range 50 {
		url := urls[index%len(urls)]
		group.Add(1)
		go func(url string) {
			defer group.Done()
			<-start
			request, err := http.NewRequest(http.MethodPost, url+"/wagering/transactions", bytes.NewReader(requestBody))
			if err != nil {
				results <- result{Err: err}
				return
			}
			request.Header.Set("Authorization", "Bearer "+providerToken)
			request.Header.Set("Idempotency-Key", "provider-a:three-api-"+wallet.ID)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				results <- result{Err: err}
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				results <- result{Err: &unexpectedHTTPStatus{got: response.StatusCode}}
				return
			}
			var decoded result
			if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
				results <- result{Err: err}
				return
			}
			results <- decoded
		}(url)
	}
	close(start)
	group.Wait()
	close(results)

	firstDeliveries, replays := 0, 0
	for result := range results {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if result.Status != "PROCESSED" {
			t.Fatalf("transaction status=%s want PROCESSED", result.Status)
		}
		if result.IdempotentReplay {
			replays++
		} else {
			firstDeliveries++
		}
	}
	if firstDeliveries != 1 || replays != 49 {
		t.Fatalf("first deliveries=%d replays=%d want 1/49", firstDeliveries, replays)
	}

	var state struct {
		Balance struct {
			Amount string `json:"amount"`
		} `json:"balance"`
	}
	getJSON(t, urls[2]+"/wallets/"+wallet.ID, internalHeaders, &state)
	if state.Balance.Amount != "75.00" {
		t.Fatalf("wallet balance=%s want 75.00", state.Balance.Amount)
	}
	var ledger struct {
		Entries []struct {
			Direction string `json:"direction"`
		} `json:"entries"`
	}
	getJSON(t, urls[2]+"/wallets/"+wallet.ID+"/ledger", internalHeaders, &ledger)
	debits := 0
	for _, entry := range ledger.Entries {
		if entry.Direction == "DEBIT" {
			debits++
		}
	}
	if debits != 1 {
		t.Fatalf("ledger debits=%d want 1", debits)
	}
}

type multiInstanceWallet struct {
	ID       string `json:"id"`
	PlayerID string `json:"playerId"`
}

func createMultiInstanceWallet(t *testing.T, baseURL string, headers map[string]string) multiInstanceWallet {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"providerId": "provider-a", "playerId": "00000000-0000-0000-0000-" + uniqueTestID(),
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/wallets", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("open wallet status=%s", response.Status)
	}
	var wallet multiInstanceWallet
	if err := json.NewDecoder(response.Body).Decode(&wallet); err != nil {
		t.Fatal(err)
	}
	return wallet
}

func getJSON(t *testing.T, url string, headers map[string]string, target any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%s", url, response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

type unexpectedHTTPStatus struct{ got int }

func (e *unexpectedHTTPStatus) Error() string { return http.StatusText(e.got) }

func uniqueTestID() string {
	return fmt.Sprintf("%012d", time.Now().UTC().UnixNano()%1_000_000_000_000)
}
