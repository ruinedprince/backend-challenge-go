package app

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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
	logger    *slog.Logger
	metrics   *Metrics
}

func NewAPI(processor *core.Processor, verifier *auth.Verifier, logger *slog.Logger, metrics *Metrics) *API {
	return &API{processor: processor, verifier: verifier, logger: logger, metrics: metrics}
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
	if !ok || !a.requireInternal(writer, principal) {
		return
	}
	var body struct {
		ProviderID     string `json:"providerId"`
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
	if strings.TrimSpace(body.ProviderID) == "" {
		writeAPIError(writer, http.StatusBadRequest, "PROVIDER_REQUIRED", "providerId is required")
		return
	}
	wallet, err := a.processor.OpenWallet(request.Context(), body.ProviderID, body.PlayerID, money)
	if err != nil {
		writeAPIError(writer, http.StatusConflict, "WALLET_CONFLICT", "wallet could not be opened")
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"id": wallet.ID, "playerId": wallet.PlayerID, "balance": wallet.Balance, "version": wallet.Version})
}

func (a *API) GetWallet(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok || !a.requireInternal(writer, principal) {
		return
	}
	wallet, err := a.processor.FindWalletInternal(request.Context(), request.PathValue("walletId"))
	if err != nil {
		a.writeWalletError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"id": wallet.ID, "playerId": wallet.PlayerID, "balance": wallet.Balance, "version": wallet.Version})
}

func (a *API) GetLedger(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok || !a.requireInternal(writer, principal) {
		return
	}
	limit := 50
	if rawLimit := request.URL.Query().Get("limit"); rawLimit != "" {
		var err error
		limit, err = strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > 50 {
			writeAPIError(writer, http.StatusBadRequest, "INVALID_LEDGER_PAGE", "limit must be between 1 and 50")
			return
		}
	}
	page, err := a.processor.ListLedgerInternal(request.Context(), request.PathValue("walletId"), request.URL.Query().Get("cursor"), limit)
	if err != nil {
		if errors.Is(err, core.ErrInvalidLedgerCursor) {
			writeAPIError(writer, http.StatusBadRequest, "INVALID_LEDGER_PAGE", "cursor is invalid")
			return
		}
		a.writeWalletError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"entries": page.Entries, "nextCursor": page.NextCursor})
}

func (a *API) ReconcileWallet(writer http.ResponseWriter, request *http.Request) {
	principal, ok := a.authenticate(writer, request)
	if !ok || !a.requireInternal(writer, principal) {
		return
	}
	wallet, calculated, count, err := a.processor.ReconcileInternal(request.Context(), request.PathValue("walletId"))
	if err != nil {
		a.writeWalletError(writer, err)
		return
	}
	difference, _ := wallet.Balance.Subtract(calculated)
	consistent := difference.IsZero()
	if !consistent {
		a.metrics.RecordReconciliationDivergence()
		a.logger.Error("wallet reconciliation divergence", "walletId", wallet.ID, "storedBalance", wallet.Balance.String(), "calculatedBalance", calculated.String(), "difference", difference.String())
	}
	writeJSON(writer, http.StatusOK, map[string]any{"walletId": wallet.ID, "storedBalance": wallet.Balance, "calculatedBalance": calculated, "difference": difference, "consistent": consistent, "checkedEntries": count})
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

func (a *API) requireInternal(writer http.ResponseWriter, principal auth.Principal) bool {
	if principal.Internal {
		return true
	}
	writeAPIError(writer, http.StatusForbidden, "INTERNAL_SERVICE_REQUIRED", "wallet operations require the internal service identity")
	return false
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
