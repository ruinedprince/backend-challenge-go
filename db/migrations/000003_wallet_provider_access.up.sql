CREATE TABLE wallet_provider_access (
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    provider_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (wallet_id, provider_id)
);

CREATE INDEX wallet_provider_access_provider_idx ON wallet_provider_access (provider_id, wallet_id);
