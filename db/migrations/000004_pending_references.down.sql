DROP INDEX IF EXISTS wager_transactions_pending_reference_idx;
ALTER TABLE wager_transactions
    DROP COLUMN IF EXISTS reference_next_attempt_at,
    DROP COLUMN IF EXISTS reference_attempts;
