package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ruinedprince/backend-challenge-go/internal/domain"
)

type WalletView struct {
	ID       string
	PlayerID string
	Balance  domain.Money
	Version  int64
}

type LedgerView struct {
	TransactionID string
	Direction     domain.LedgerDirection
	Money         domain.Money
	BalanceBefore domain.Money
	BalanceAfter  domain.Money
	CreatedAt     time.Time
}

func (p *Processor) OpenWallet(ctx context.Context, providerID, playerID string, initial domain.Money) (WalletView, error) {
	walletID, err := newUUID()
	if err != nil {
		return WalletView{}, err
	}
	now := p.now().UTC()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return WalletView{}, fmt.Errorf("begin wallet opening: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO wallets (id, player_id, currency, balance_minor_units, version, created_at, updated_at) VALUES ($1,$2,$3,$4,1,$5,$5)`, walletID, playerID, initial.Currency(), initial.MinorUnits(), now); err != nil {
		return WalletView{}, fmt.Errorf("create wallet: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_provider_access (wallet_id, provider_id) VALUES ($1,$2)`, walletID, providerID); err != nil {
		return WalletView{}, fmt.Errorf("grant wallet provider access: %w", err)
	}
	if initial.IsPositive() {
		transactionID, err := newUUID()
		if err != nil {
			return WalletView{}, err
		}
		if _, err = tx.Exec(ctx, `
            INSERT INTO wager_transactions (id,wallet_id,player_id,transaction_type,amount_minor_units,currency,state,result_balance_minor_units,result_wallet_version,occurred_at,created_at,updated_at)
            VALUES ($1,$2,$3,'OPENING',$4,$5,'PROCESSED',$4,1,$6,$6,$6)
        `, transactionID, walletID, playerID, initial.MinorUnits(), initial.Currency(), now); err != nil {
			return WalletView{}, fmt.Errorf("record wallet opening: %w", err)
		}
		if _, err = tx.Exec(ctx, `
            INSERT INTO wallet_ledger_entries (wallet_id,transaction_id,direction,amount_minor_units,currency,balance_before_minor_units,balance_after_minor_units,created_at)
            VALUES ($1,$2,'CREDIT',$3,$4,0,$3,$5)
        `, walletID, transactionID, initial.MinorUnits(), initial.Currency(), now); err != nil {
			return WalletView{}, fmt.Errorf("record opening ledger: %w", err)
		}
		if err = insertOpeningOutbox(ctx, tx, transactionID, walletID, now); err != nil {
			return WalletView{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return WalletView{}, fmt.Errorf("commit wallet opening: %w", err)
	}
	return WalletView{ID: walletID, PlayerID: playerID, Balance: initial, Version: 1}, nil
}

func (p *Processor) FindWallet(ctx context.Context, providerID, walletID string) (WalletView, error) {
	var view WalletView
	var minor int64
	var currency string
	err := p.pool.QueryRow(ctx, `
        SELECT w.id,w.player_id,w.balance_minor_units,w.currency,w.version FROM wallets w
        JOIN wallet_provider_access a ON a.wallet_id=w.id
        WHERE w.id=$1 AND a.provider_id=$2
    `, walletID, providerID).Scan(&view.ID, &view.PlayerID, &minor, &currency, &view.Version)
	if err != nil {
		return WalletView{}, err
	}
	money, err := domain.NewMoney(minor, currency)
	if err != nil {
		return WalletView{}, err
	}
	view.Balance = money
	return view, nil
}

func (p *Processor) ListLedger(ctx context.Context, providerID, walletID string, limit int) ([]LedgerView, error) {
	if limit < 1 || limit > 50 {
		limit = 50
	}
	rows, err := p.pool.Query(ctx, `
        SELECT e.transaction_id,e.direction,e.amount_minor_units,e.currency,e.balance_before_minor_units,e.balance_after_minor_units,e.created_at
        FROM wallet_ledger_entries e JOIN wallet_provider_access a ON a.wallet_id=e.wallet_id
        WHERE e.wallet_id=$1 AND a.provider_id=$2 ORDER BY e.created_at DESC,e.id DESC LIMIT $3
    `, walletID, providerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []LedgerView
	for rows.Next() {
		var item LedgerView
		var amount, before, after int64
		var currency string
		if err := rows.Scan(&item.TransactionID, &item.Direction, &amount, &currency, &before, &after, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Money, _ = domain.NewMoney(amount, currency)
		item.BalanceBefore, _ = domain.NewMoney(before, currency)
		item.BalanceAfter, _ = domain.NewMoney(after, currency)
		entries = append(entries, item)
	}
	return entries, rows.Err()
}

func (p *Processor) Reconcile(ctx context.Context, providerID, walletID string) (WalletView, domain.Money, int64, error) {
	wallet, err := p.FindWallet(ctx, providerID, walletID)
	if err != nil {
		return WalletView{}, domain.Money{}, 0, err
	}
	var calculated int64
	var count int64
	err = p.pool.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN direction='CREDIT' THEN amount_minor_units ELSE -amount_minor_units END),0),count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&calculated, &count)
	if err != nil {
		return WalletView{}, domain.Money{}, 0, err
	}
	money, err := domain.NewMoney(calculated, wallet.Balance.Currency())
	return wallet, money, count, err
}

func insertOpeningOutbox(ctx context.Context, tx pgx.Tx, transactionID, walletID string, now time.Time) error {
	payload, _ := json.Marshal(map[string]string{"transactionId": transactionID, "walletId": walletID})
	for _, eventType := range []string{"WagerTransactionProcessed", "WalletBalanceChanged"} {
		if _, err := tx.Exec(ctx, `INSERT INTO outbox_events (aggregate_id,event_type,payload,occurred_at) VALUES ($1,$2,$3,$4)`, transactionID, eventType, payload, now); err != nil {
			return fmt.Errorf("insert opening outbox: %w", err)
		}
	}
	return nil
}
