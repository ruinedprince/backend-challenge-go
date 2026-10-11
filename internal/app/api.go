package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ruinedprince/backend-challenge-go/internal/auth"
	"github.com/ruinedprince/backend-challenge-go/internal/core"
	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

type API struct {
	processor *core.Processor
	verifier  *auth.Verifier
}

func NewAPI(processor *core.Processor, verifier *auth.Verifier) *API {
	return &API{processor: processor, verifier: verifier}
}

func (a *API) CreateTransaction(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok {
		return
	}
	key := request.Header.Get("Idempotency-Key")
	if strings.TrimSpace(key) == "" {
		writeAPIError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
		return
	}
	var body struct {
		ProviderID                     string                 `json:"providerId"`
		ExternalTransactionID          string                 `json:"externalTransactionId"`
		PlayerID                       string                 `json:"playerId"`
		WalletID                       string                 `json:"walletId"`
		RoundID                        string                 `json:"roundId"`
		GameID                         string                 `json:"gameId"`
		ReferenceExternalTransactionID string                 `json:"referenceExternalTransactionId"`
		Kind                           domain.TransactionType `json:"kind"`
		Money                          struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"money"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_BODY", "request body must be valid JSON")
		return
	}
	if body.ProviderID != principal.ProviderID {
		writeAPIError(writer, http.StatusForbidden, "PROVIDER_FORBIDDEN", "token cannot act as this provider")
		return
	}
	if body.Kind == domain.TransactionOpening {
		writeAPIError(writer, http.StatusBadRequest, "INTERNAL_OPERATION", "OPENING cannot be submitted externally")
		return
	}
	money, err := domain.ParseExternalMoney(body.Money.Amount, body.Money.Currency)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_MONEY", err.Error())
		return
	}
	result, err := a.processor.Process(request.Context(), core.TransactionRequest{
		ProviderID: body.ProviderID, ExternalTransactionID: body.ExternalTransactionID, IdempotencyKey: key,
		PlayerID: body.PlayerID, WalletID: body.WalletID, RoundID: body.RoundID, GameID: body.GameID,
		Type: body.Kind, Money: money, ReferenceExternalTransactionID: body.ReferenceExternalTransactionID, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		a.writeProcessError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, transactionResponse(result))
}

func (a *API) GetTransaction(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok {
		return
	}
	view, err := a.processor.FindTransaction(request.Context(), principal.ProviderID, request.PathValue("transactionId"))
	a.writeTransactionView(writer, view, err)
}

func (a *API) GetExternalTransaction(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok {
		return
	}
	providerID := request.PathValue("providerId")
	if providerID != principal.ProviderID {
		writeAPIError(writer, http.StatusForbidden, "PROVIDER_FORBIDDEN", "token cannot access this provider")
		return
	}
	view, err := a.processor.FindExternalTransaction(request.Context(), providerID, request.PathValue("externalTransactionId"))
	a.writeTransactionView(writer, view, err)
}

func (a *API) OpenWallet(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok {
		return
	}
	var body struct {
		PlayerID       string `json:"playerId"`
		InitialBalance struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"initialBalance"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_BODY", "request body must be valid JSON")
		return
	}
	money, err := domain.ParseExternalMoney(body.InitialBalance.Amount, body.InitialBalance.Currency)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_MONEY", err.Error())
		return
	}
	wallet, err := a.processor.OpenWallet(request.Context(), principal.ProviderID, body.PlayerID, money)
	if err != nil {
		writeAPIError(writer, http.StatusConflict, "WALLET_CONFLICT", "wallet could not be opened")
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"id": wallet.ID, "playerId": wallet.PlayerID, "balance": wallet.Balance, "version": wallet.Version})
}

func (a *API) GetWallet(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok {
		return
	}
	wallet, err := a.processor.FindWallet(request.Context(), principal.ProviderID, request.PathValue("walletId"))
	if err != nil {
		a.writeWalletError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"id": wallet.ID, "playerId": wallet.PlayerID, "balance": wallet.Balance, "version": wallet.Version})
}

func (a *API) GetLedger(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok {
		return
	}
	entries, err := a.processor.ListLedger(request.Context(), principal.ProviderID, request.PathValue("walletId"), 50)
	if err != nil {
		a.writeWalletError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"entries": entries})
}

func (a *API) ReconcileWallet(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok {
		return
	}
	wallet, calculated, count, err := a.processor.Reconcile(request.Context(), principal.ProviderID, request.PathValue("walletId"))
	if err != nil {
		a.writeWalletError(writer, err)
		return
	}
	difference, _ := wallet.Balance.Subtract(calculated)
	writeJSON(writer, http.StatusOK, map[string]any{"walletId": wallet.ID, "storedBalance": wallet.Balance, "calculatedBalance": calculated, "difference": difference, "consistent": difference.IsZero(), "checkedEntries": count})
}

func (a *API) writeTransactionView(writer http.ResponseWriter, view core.TransactionView, err error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(writer, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
			return
		}
		writeAPIError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "could not read transaction")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"transactionId": view.ID, "status": view.State, "balance": view.Balance, "failureCode": view.FailureCode,
	})
}

func (a *API) writeWalletError(writer http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeAPIError(writer, http.StatusNotFound, "WALLET_NOT_FOUND", "wallet not found")
		return
	}
	writeAPIError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "could not read wallet")
}

func (a *API) authenticate(writer http.ResponseWriter, request *http.Request) (auth.Principal, bool) {
	principal, err := a.verifier.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "valid bearer token required")
		return auth.Principal{}, false
	}
	return principal, true
}

func (a *API) writeProcessError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrIdempotencyConflict), errors.Is(err, core.ErrExternalIdentityConflict):
		writeAPIError(writer, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error())
	case errors.Is(err, core.ErrProviderWalletForbidden):
		writeAPIError(writer, http.StatusForbidden, "PROVIDER_FORBIDDEN", "token cannot access this wallet")
	case errors.Is(err, domain.ErrInvalidAmount), errors.Is(err, domain.ErrInvalidCurrency), errors.Is(err, domain.ErrInvalidTransaction), errors.Is(err, domain.ErrInvalidWallet):
		writeAPIError(writer, http.StatusBadRequest, "INVALID_TRANSACTION", err.Error())
	default:
		writeAPIError(writer, http.StatusServiceUnavailable, "TRANSIENT_UNAVAILABLE", "transaction could not be processed")
	}
}

func transactionResponse(result core.TransactionResult) map[string]any {
	return map[string]any{
		"transactionId":    result.TransactionID,
		"status":           result.Status,
		"balance":          result.Balance,
		"idempotentReplay": result.IdempotentReplay,
		"failureCode":      result.FailureCode,
	}
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]string{"code": code, "message": message})
}
