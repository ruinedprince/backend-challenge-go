package core

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

type TransactionView struct {
	ID          string
	ProviderID  string
	State       domain.TransactionState
	FailureCode string
	Balance     domain.Money
}

func (p *Processor) FindTransaction(ctx context.Context, providerID, transactionID string) (TransactionView, error) {
	return findTransaction(ctx, p.pool, `
        SELECT id, provider_id, state, failure_code, result_balance_minor_units, currency
        FROM wager_transactions
        WHERE id = $1 AND provider_id = $2
    `, transactionID, providerID)
}

func (p *Processor) FindExternalTransaction(ctx context.Context, providerID, externalTransactionID string) (TransactionView, error) {
	return findTransaction(ctx, p.pool, `
        SELECT id, provider_id, state, failure_code, result_balance_minor_units, currency
        FROM wager_transactions
        WHERE provider_id = $1 AND external_transaction_id = $2
    `, providerID, externalTransactionID)
}

func findTransaction(ctx context.Context, pool *pgxpool.Pool, query string, first, second string) (TransactionView, error) {
	var view TransactionView
	var balanceMinorUnits *int64
	var failureCode *string
	var currency string
	if err := pool.QueryRow(ctx, query, first, second).Scan(&view.ID, &view.ProviderID, &view.State, &failureCode, &balanceMinorUnits, &currency); err != nil {
		if err == pgx.ErrNoRows {
			return TransactionView{}, err
		}
		return TransactionView{}, fmt.Errorf("find transaction: %w", err)
	}
	if balanceMinorUnits == nil {
		return TransactionView{}, fmt.Errorf("transaction %s has no recorded result", view.ID)
	}
	balance, err := domain.NewMoney(*balanceMinorUnits, currency)
	if err != nil {
		return TransactionView{}, err
	}
	view.Balance = balance
	if failureCode != nil {
		view.FailureCode = *failureCode
	}
	return view, nil
}
