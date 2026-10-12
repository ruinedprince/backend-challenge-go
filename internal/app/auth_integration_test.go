package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ruinedprince/backend-challenge-go/internal/auth"
	"github.com/ruinedprince/backend-challenge-go/internal/core"
	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

func TestAPIAuthenticationAndInternalAuthorizationWithKeycloak(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("KEYCLOAK_ISSUER_URL") == "" {
		t.Skip("PostgreSQL and Keycloak are required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, "TRUNCATE outbox_events, inbox_messages, wallet_ledger_entries, wager_transactions, wallets CASCADE"); err != nil {
		t.Fatal(err)
	}
	providerToken := keycloakClientToken(t, "provider-a", "provider-a-secret")
	api := NewAPI(core.NewProcessor(pool), auth.NewVerifier(os.Getenv("KEYCLOAK_ISSUER_URL"), "wallet-service"), slog.Default(), NewMetrics(pool))
	body := []byte(`{"providerId":"provider-a","externalTransactionId":"unauthorized","playerId":"00000000-0000-0000-0000-000000000981","walletId":"00000000-0000-0000-0000-000000000982","roundId":"round","gameId":"game","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}`)
	for _, authorization := range []string{"", "Bearer not-a-valid-token"} {
		req := httptest.NewRequest(http.MethodPost, "/wagering/transactions", bytes.NewReader(body))
		req.Header.Set("Idempotency-Key", "provider-a:unauthorized")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		recorder := httptest.NewRecorder()
		api.CreateTransaction(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %q status=%d want 401", authorization, recorder.Code)
		}
	}
	expiredToken := keycloakExpiredClientToken(t)
	expired := httptest.NewRequest(http.MethodPost, "/wagering/transactions", bytes.NewReader(body))
	expired.Header.Set("Authorization", "Bearer "+expiredToken)
	expired.Header.Set("Idempotency-Key", "provider-a:expired")
	expiredRecorder := httptest.NewRecorder()
	api.CreateTransaction(expiredRecorder, expired)
	if expiredRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired token status=%d want 401", expiredRecorder.Code)
	}
	var transactions int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM wager_transactions").Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if transactions != 0 {
		t.Fatalf("unauthorized calls created %d transactions", transactions)
	}
	open := httptest.NewRequest(http.MethodPost, "/wallets", bytes.NewReader([]byte(`{"providerId":"provider-a","playerId":"00000000-0000-0000-0000-000000000981","initialBalance":{"amount":"0.00","currency":"BRL"}}`)))
	open.Header.Set("Authorization", "Bearer "+providerToken)
	openRecorder := httptest.NewRecorder()
	api.OpenWallet(openRecorder, open)
	if openRecorder.Code != http.StatusForbidden {
		t.Fatalf("provider opening wallet status=%d want 403", openRecorder.Code)
	}

	zero, err := domain.ParseExternalMoney("0.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := core.NewProcessor(pool).OpenWallet(ctx, "provider-b", "00000000-0000-0000-0000-000000000983", zero)
	if err != nil {
		t.Fatal(err)
	}
	isolationBody := []byte(`{"providerId":"provider-a","externalTransactionId":"provider-isolation","playerId":"00000000-0000-0000-0000-000000000983","walletId":"` + wallet.ID + `","roundId":"round","gameId":"game","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}`)
	isolation := httptest.NewRequest(http.MethodPost, "/wagering/transactions", bytes.NewReader(isolationBody))
	isolation.Header.Set("Authorization", "Bearer "+providerToken)
	isolation.Header.Set("Idempotency-Key", "provider-a:provider-isolation")
	isolationRecorder := httptest.NewRecorder()
	api.CreateTransaction(isolationRecorder, isolation)
	if isolationRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-provider transaction status=%d want 403", isolationRecorder.Code)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM wager_transactions").Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if transactions != 0 {
		t.Fatalf("cross-provider call created %d transactions", transactions)
	}
}

func keycloakClientToken(t *testing.T, clientID, secret string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequest(http.MethodPost, os.Getenv("KEYCLOAK_ISSUER_URL")+"/protocol/openid-connect/token", bytes.NewBufferString(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(clientID, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("token status=%s", response.Status)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.AccessToken == "" {
		t.Fatal("Keycloak returned an empty token")
	}
	return body.AccessToken
}

func keycloakExpiredClientToken(t *testing.T) string {
	t.Helper()
	issuer := os.Getenv("KEYCLOAK_ISSUER_URL")
	adminURL := issuer[:len(issuer)-len("/realms/wagering")] + "/realms/master/protocol/openid-connect/token"
	form := url.Values{"client_id": {"admin-cli"}, "grant_type": {"password"}, "username": {"admin"}, "password": {"admin"}}
	request, err := http.NewRequest(http.MethodPost, adminURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Keycloak admin token status=%s", response.Status)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}

	clientID := "expired-token-test"
	secret := "expired-token-test-secret"
	client := map[string]any{
		"clientId": clientID, "enabled": true, "publicClient": false, "secret": secret,
		"serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false,
		"protocol": "openid-connect", "attributes": map[string]string{"access.token.lifespan": "1"},
	}
	payload, err := json.Marshal(client)
	if err != nil {
		t.Fatal(err)
	}
	createURL := issuer[:len(issuer)-len("/realms/wagering")] + "/admin/realms/wagering/clients"
	create, err := http.NewRequest(http.MethodPost, createURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	create.Header.Set("Authorization", "Bearer "+token.AccessToken)
	create.Header.Set("Content-Type", "application/json")
	created, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create expiring Keycloak client status=%s", created.Status)
	}
	location := created.Header.Get("Location")
	if location == "" {
		t.Fatal("Keycloak did not return the created client location")
	}
	t.Cleanup(func() {
		deleteRequest, err := http.NewRequest(http.MethodDelete, location, nil)
		if err != nil {
			t.Error(err)
			return
		}
		deleteRequest.Header.Set("Authorization", "Bearer "+token.AccessToken)
		if response, err := http.DefaultClient.Do(deleteRequest); err != nil {
			t.Error(err)
		} else {
			response.Body.Close()
		}
	})

	expiringToken := keycloakClientToken(t, clientID, secret)
	time.Sleep(2 * time.Second)
	return expiringToken
}
