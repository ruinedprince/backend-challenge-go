CREATE TABLE accounting_postings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    journal_id UUID NOT NULL,
    ledger_entry_id UUID NOT NULL REFERENCES wallet_ledger_entries(id),
    account_code TEXT NOT NULL CHECK (account_code IN ('PLAYER_WALLET', 'HOUSE_CLEARING')),
    side TEXT NOT NULL CHECK (side IN ('DEBIT', 'CREDIT')),
    amount_minor_units BIGINT NOT NULL CHECK (amount_minor_units > 0),
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ledger_entry_id, account_code),
    UNIQUE (journal_id, side)
);

CREATE OR REPLACE FUNCTION post_double_entry_for_wallet_ledger()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE entry_journal_id UUID := gen_random_uuid();
BEGIN
    INSERT INTO accounting_postings (journal_id, ledger_entry_id, account_code, side, amount_minor_units, currency)
    VALUES
      (entry_journal_id, NEW.id, 'PLAYER_WALLET', CASE WHEN NEW.direction = 'DEBIT' THEN 'CREDIT' ELSE 'DEBIT' END, NEW.amount_minor_units, NEW.currency),
      (entry_journal_id, NEW.id, 'HOUSE_CLEARING', CASE WHEN NEW.direction = 'DEBIT' THEN 'DEBIT' ELSE 'CREDIT' END, NEW.amount_minor_units, NEW.currency);
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallet_ledger_entries_double_entry AFTER INSERT ON wallet_ledger_entries FOR EACH ROW EXECUTE FUNCTION post_double_entry_for_wallet_ledger();
CREATE OR REPLACE FUNCTION reject_accounting_posting_mutation() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'accounting_postings are append-only'; END; $$;
CREATE TRIGGER accounting_postings_no_update BEFORE UPDATE ON accounting_postings FOR EACH ROW EXECUTE FUNCTION reject_accounting_posting_mutation();
CREATE TRIGGER accounting_postings_no_delete BEFORE DELETE ON accounting_postings FOR EACH ROW EXECUTE FUNCTION reject_accounting_posting_mutation();
