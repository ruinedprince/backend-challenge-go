\set ON_ERROR_STOP on

BEGIN;

INSERT INTO wallets (id, player_id, currency, balance_minor_units)
VALUES ('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000011', 'BRL', 10000);

INSERT INTO wager_transactions (
    id, provider_id, external_transaction_id, idempotency_key, payload_hash,
    wallet_id, player_id, round_id, game_id, transaction_type, amount_minor_units,
    currency, state, occurred_at
) VALUES (
    '00000000-0000-0000-0000-000000000101', 'provider-a', 'external-1', 'provider-a:external-1', 'hash-1',
    '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000011', 'round-1', 'game-1', 'BET', 2500,
    'BRL', 'PROCESSED', now()
);

INSERT INTO wallet_ledger_entries (
    wallet_id, transaction_id, direction, amount_minor_units, currency,
    balance_before_minor_units, balance_after_minor_units
) VALUES (
    '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000101', 'DEBIT', 2500, 'BRL', 10000, 7500
);

DO $$
BEGIN
    BEGIN
        INSERT INTO wallets (player_id, currency, balance_minor_units)
        VALUES ('00000000-0000-0000-0000-000000000011', 'BRL', 0);
        RAISE EXCEPTION 'expected duplicate wallet constraint';
    EXCEPTION WHEN unique_violation THEN NULL;
    END;

    BEGIN
        UPDATE wallet_ledger_entries SET amount_minor_units = 1;
        RAISE EXCEPTION 'expected immutable ledger trigger';
    EXCEPTION WHEN raise_exception THEN NULL;
    END;

    BEGIN
        INSERT INTO wallet_ledger_entries (
            wallet_id, transaction_id, direction, amount_minor_units, currency,
            balance_before_minor_units, balance_after_minor_units
        ) VALUES (
            '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000101', 'CREDIT', 100, 'BRL', 7500, 7601
        );
        RAISE EXCEPTION 'expected ledger arithmetic constraint';
    EXCEPTION WHEN check_violation THEN NULL;
    END;

    BEGIN
        INSERT INTO wager_transactions (
            provider_id, external_transaction_id, idempotency_key, payload_hash,
            wallet_id, player_id, round_id, game_id, transaction_type, amount_minor_units,
            currency, state, occurred_at
        ) VALUES (
            'provider-a', 'external-wrong-currency', 'provider-a:wrong-currency', 'hash-2',
            '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000011', 'round-1', 'game-1', 'BET', 100,
            'USD', 'PENDING', now()
        );
        RAISE EXCEPTION 'expected transaction wallet identity trigger';
    EXCEPTION WHEN raise_exception THEN NULL;
    END;
END;
$$;

ROLLBACK;
