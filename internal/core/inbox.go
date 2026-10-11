package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInboxPayloadConflict = errors.New("inbox message id was redelivered with different payload")

// ProcessInbox commits the inbox receipt, financial movement and completion
// marker atomically. A redelivered message becomes a no-op.
func (p *Processor) ProcessInbox(ctx context.Context, consumerName, messageID, payloadHash string, request TransactionRequest) (TransactionResult, bool, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TransactionResult{}, false, fmt.Errorf("begin inbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `INSERT INTO inbox_messages (consumer_name,message_id,payload_hash) VALUES ($1,$2,$3) ON CONFLICT (consumer_name,message_id) DO NOTHING`, consumerName, messageID, payloadHash)
	if err != nil {
		return TransactionResult{}, false, fmt.Errorf("record inbox message: %w", err)
	}
	if command.RowsAffected() == 0 {
		var persistedHash string
		if err := tx.QueryRow(ctx, `SELECT payload_hash FROM inbox_messages WHERE consumer_name=$1 AND message_id=$2`, consumerName, messageID).Scan(&persistedHash); err != nil {
			return TransactionResult{}, false, err
		}
		if persistedHash != payloadHash {
			return TransactionResult{}, false, ErrInboxPayloadConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return TransactionResult{}, false, err
		}
		return TransactionResult{}, true, nil
	}
	result, err := p.processInTransaction(ctx, tx, request)
	if err != nil {
		return TransactionResult{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE inbox_messages SET completed_at=$1 WHERE consumer_name=$2 AND message_id=$3`, time.Now().UTC(), consumerName, messageID); err != nil {
		return TransactionResult{}, false, fmt.Errorf("complete inbox message: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return TransactionResult{}, false, fmt.Errorf("commit inbox transaction: %w", err)
	}
	return result, false, nil
}
