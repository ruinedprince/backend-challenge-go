CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE wallets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    player_id UUID NOT NULL,
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor_units BIGINT NOT NULL DEFAULT 0 CHECK (balance_minor_units >= 0),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (player_id, currency)
);

CREATE TABLE wager_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id TEXT,
    external_transaction_id TEXT,
    idempotency_key TEXT,
    payload_hash TEXT,
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    player_id UUID NOT NULL,
    round_id TEXT,
    game_id TEXT,
    transaction_type TEXT NOT NULL CHECK (transaction_type IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    amount_minor_units BIGINT NOT NULL CHECK (amount_minor_units >= 0),
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    reference_external_transaction_id TEXT,
    reference_transaction_id UUID REFERENCES wager_transactions(id),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    failure_code TEXT,
    result_balance_minor_units BIGINT CHECK (result_balance_minor_units >= 0),
    result_wallet_version BIGINT CHECK (result_wallet_version >= 1),
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (transaction_type = 'LOSS' AND amount_minor_units = 0)
        OR (transaction_type IN ('BET', 'WIN', 'REFUND', 'ROLLBACK') AND amount_minor_units > 0)
        OR (transaction_type = 'OPENING')
    ),
    CHECK (
        (transaction_type IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR (transaction_type NOT IN ('REFUND', 'ROLLBACK'))
    ),
    CHECK (
        (state IN ('REJECTED', 'FAILED') AND failure_code IS NOT NULL)
        OR (state NOT IN ('REJECTED', 'FAILED') AND failure_code IS NULL)
    ),
    CHECK (
        (
            transaction_type = 'OPENING'
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND payload_hash IS NULL
            AND round_id IS NULL
            AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
            AND reference_transaction_id IS NULL
        )
        OR (
            transaction_type <> 'OPENING'
            AND provider_id IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL
            AND game_id IS NOT NULL
        )
    )
);

-- The external operation identity is authoritative even if a caller retries
-- with a different idempotency key.
CREATE UNIQUE INDEX wager_transactions_external_identity_unique
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE provider_id IS NOT NULL;

CREATE UNIQUE INDEX wager_transactions_idempotency_key_unique
    ON wager_transactions (provider_id, idempotency_key)
    WHERE provider_id IS NOT NULL;

-- Only one successful reversal of each kind may target an external operation.
CREATE UNIQUE INDEX wager_transactions_processed_refund_unique
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE transaction_type = 'REFUND' AND state = 'PROCESSED';

CREATE UNIQUE INDEX wager_transactions_processed_rollback_unique
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE transaction_type = 'ROLLBACK' AND state = 'PROCESSED';

CREATE INDEX wager_transactions_wallet_id_created_at_idx
    ON wager_transactions (wallet_id, created_at DESC);

CREATE OR REPLACE FUNCTION validate_transaction_wallet_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    persisted_player_id UUID;
    persisted_currency CHAR(3);
BEGIN
    SELECT player_id, currency
      INTO persisted_player_id, persisted_currency
      FROM wallets
     WHERE id = NEW.wallet_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'wallet % does not exist', NEW.wallet_id;
    END IF;
    IF NEW.player_id <> persisted_player_id OR NEW.currency <> persisted_currency THEN
        RAISE EXCEPTION 'transaction must match its wallet player and currency';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transactions_wallet_identity
    BEFORE INSERT OR UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION validate_transaction_wallet_identity();

CREATE TABLE wallet_ledger_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    transaction_id UUID NOT NULL REFERENCES wager_transactions(id),
    direction TEXT NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor_units BIGINT NOT NULL CHECK (amount_minor_units > 0),
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_before_minor_units BIGINT NOT NULL CHECK (balance_before_minor_units >= 0),
    balance_after_minor_units BIGINT NOT NULL CHECK (balance_after_minor_units >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (wallet_id, transaction_id),
    CHECK (
        (direction = 'DEBIT' AND balance_after_minor_units = balance_before_minor_units - amount_minor_units)
        OR (direction = 'CREDIT' AND balance_after_minor_units = balance_before_minor_units + amount_minor_units)
    )
);

CREATE OR REPLACE FUNCTION validate_ledger_wallet_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    wallet_currency CHAR(3);
    transaction_wallet_id UUID;
    transaction_currency CHAR(3);
BEGIN
    SELECT currency INTO wallet_currency FROM wallets WHERE id = NEW.wallet_id;
    SELECT wallet_id, currency INTO transaction_wallet_id, transaction_currency
      FROM wager_transactions WHERE id = NEW.transaction_id;

    IF NOT FOUND OR transaction_wallet_id <> NEW.wallet_id
       OR wallet_currency <> NEW.currency OR transaction_currency <> NEW.currency THEN
        RAISE EXCEPTION 'ledger entry must match its wallet and transaction currency';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallet_ledger_entries_wallet_identity
    BEFORE INSERT OR UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION validate_ledger_wallet_identity();

CREATE OR REPLACE FUNCTION reject_wallet_ledger_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries are append-only';
END;
$$;

CREATE TRIGGER wallet_ledger_entries_no_update
    BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_wallet_ledger_mutation();

CREATE TRIGGER wallet_ledger_entries_no_delete
    BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_wallet_ledger_mutation();

CREATE TABLE inbox_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consumer_name TEXT NOT NULL,
    message_id TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    UNIQUE (consumer_name, message_id)
);

CREATE TABLE outbox_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    claimed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, created_at)
    WHERE published_at IS NULL;
