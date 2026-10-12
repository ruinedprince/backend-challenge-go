package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	entryID       string
}

type LedgerPage struct {
	Entries    []LedgerView
	NextCursor string
}

var ErrInvalidLedgerCursor = errors.New("invalid ledger cursor")

type ledgerCursor struct {
	CreatedAt time.Time `json:"createdAt"`
	EntryID   string    `json:"entryId"`
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
		if err = insertOpeningOutbox(ctx, tx, transactionID, walletID, playerID, providerID, initial, now); err != nil {
			return WalletView{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return WalletView{}, fmt.Errorf("commit wallet opening: %w", err)
	}
	return WalletView{ID: walletID, PlayerID: playerID, Balance: initial, Version: 1}, nil
}

func (p *Processor) FindWallet(ctx context.Context, providerID, walletID string) (WalletView, error) {
	return p.findWallet(ctx, walletID, providerID)
}

func (p *Processor) FindWalletInternal(ctx context.Context, walletID string) (WalletView, error) {
	return p.findWallet(ctx, walletID, "")
}

func (p *Processor) findWallet(ctx context.Context, walletID, providerID string) (WalletView, error) {
	return findWallet(ctx, p.pool, walletID, providerID)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func findWallet(ctx context.Context, queryer rowQuerier, walletID, providerID string) (WalletView, error) {
	var view WalletView
	var minor int64
	var currency string
	query := `
        SELECT w.id,w.player_id,w.balance_minor_units,w.currency,w.version FROM wallets w
	`
	args := []any{walletID}
	if providerID != "" {
		query += ` JOIN wallet_provider_access a ON a.wallet_id=w.id WHERE w.id=$1 AND a.provider_id=$2`
		args = append(args, providerID)
	} else {
		query += ` WHERE w.id=$1`
	}
	err := queryer.QueryRow(ctx, query, args...).Scan(&view.ID, &view.PlayerID, &minor, &currency, &view.Version)
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

func (p *Processor) ListLedger(ctx context.Context, providerID, walletID, cursor string, limit int) (LedgerPage, error) {
	return p.listLedger(ctx, walletID, providerID, cursor, limit)
}

func (p *Processor) ListLedgerInternal(ctx context.Context, walletID, cursor string, limit int) (LedgerPage, error) {
	return p.listLedger(ctx, walletID, "", cursor, limit)
}

func (p *Processor) listLedger(ctx context.Context, walletID, providerID, cursor string, limit int) (LedgerPage, error) {
	if limit < 1 || limit > 50 {
		return LedgerPage{}, fmt.Errorf("%w: limit must be between 1 and 50", ErrInvalidLedgerCursor)
	}
	position, err := decodeLedgerCursor(cursor)
	if err != nil {
		return LedgerPage{}, err
	}
	query := `
		SELECT e.id,e.transaction_id,e.direction,e.amount_minor_units,e.currency,e.balance_before_minor_units,e.balance_after_minor_units,e.created_at
	        FROM wallet_ledger_entries e`
	args := []any{walletID}
	if providerID != "" {
		query += ` JOIN wallet_provider_access a ON a.wallet_id=e.wallet_id WHERE e.wallet_id=$1 AND a.provider_id=$2`
		args = append(args, providerID)
	} else {
		query += ` WHERE e.wallet_id=$1`
	}
	if position != nil {
		query += ` AND (e.created_at,e.id) < ($` + fmt.Sprint(len(args)+1) + `,$` + fmt.Sprint(len(args)+2) + `)`
		args = append(args, position.CreatedAt, position.EntryID)
	}
	query += ` ORDER BY e.created_at DESC,e.id DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return LedgerPage{}, err
	}
	defer rows.Close()
	var page LedgerPage
	for rows.Next() {
		var item LedgerView
		var amount, before, after int64
		var currency string
		if err := rows.Scan(&item.entryID, &item.TransactionID, &item.Direction, &amount, &currency, &before, &after, &item.CreatedAt); err != nil {
			return LedgerPage{}, err
		}
		item.Money, _ = domain.NewMoney(amount, currency)
		item.BalanceBefore, _ = domain.NewMoney(before, currency)
		item.BalanceAfter, _ = domain.NewMoney(after, currency)
		page.Entries = append(page.Entries, item)
	}
	if err := rows.Err(); err != nil {
		return LedgerPage{}, err
	}
	if len(page.Entries) == limit {
		last := page.Entries[len(page.Entries)-1]
		page.NextCursor, err = encodeLedgerCursor(ledgerCursor{CreatedAt: last.CreatedAt, EntryID: last.entryID})
		if err != nil {
			return LedgerPage{}, err
		}
	}
	return page, nil
}

func (p *Processor) Reconcile(ctx context.Context, providerID, walletID string) (WalletView, domain.Money, int64, error) {
	return p.reconcile(ctx, walletID, providerID)
}

func (p *Processor) ReconcileInternal(ctx context.Context, walletID string) (WalletView, domain.Money, int64, error) {
	return p.reconcile(ctx, walletID, "")
}

func (p *Processor) reconcile(ctx context.Context, walletID, providerID string) (WalletView, domain.Money, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return WalletView{}, domain.Money{}, 0, fmt.Errorf("begin reconciliation snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	wallet, err := findWallet(ctx, tx, walletID, providerID)
	if err != nil {
		return WalletView{}, domain.Money{}, 0, err
	}
	var calculated int64
	var count int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN direction='CREDIT' THEN amount_minor_units ELSE -amount_minor_units END),0),count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&calculated, &count)
	if err != nil {
		return WalletView{}, domain.Money{}, 0, err
	}
	money, err := domain.NewMoney(calculated, wallet.Balance.Currency())
	if err != nil {
		return WalletView{}, domain.Money{}, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WalletView{}, domain.Money{}, 0, fmt.Errorf("commit reconciliation snapshot: %w", err)
	}
	return wallet, money, count, nil
}

func encodeLedgerCursor(cursor ledgerCursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeLedgerCursor(encoded string) (*ledgerCursor, error) {
	if encoded == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed encoding", ErrInvalidLedgerCursor)
	}
	var cursor ledgerCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.CreatedAt.IsZero() || cursor.EntryID == "" {
		return nil, fmt.Errorf("%w: malformed position", ErrInvalidLedgerCursor)
	}
	return &cursor, nil
}

func insertOpeningOutbox(ctx context.Context, tx pgx.Tx, transactionID, walletID, playerID, providerID string, initial domain.Money, now time.Time) error {
	payload, err := json.Marshal(eventPayload{CorrelationID: transactionID, Data: wagerEventData{TransactionID: transactionID, ProviderID: providerID, WalletID: walletID, PlayerID: playerID, Type: domain.TransactionOpening, State: domain.TransactionProcessed, Money: initial}})
	if err != nil {
		return fmt.Errorf("marshal opening outbox payload: %w", err)
	}
	if err := insertOutboxRecord(ctx, tx, transactionID, "WagerTransactionProcessed", payload, now); err != nil {
		return err
	}
	zero, _ := domain.NewMoney(0, initial.Currency())
	entry, err := domain.NewWalletLedgerEntry(walletID, transactionID, domain.LedgerCredit, initial, zero, initial, now)
	if err != nil {
		return fmt.Errorf("build opening ledger event: %w", err)
	}
	return insertWalletBalanceChangedOutboxEvent(ctx, tx, entry, 1, transactionID, now)
}
