-- Canaries detect a missing/replaced master key before serving secret operations.
CREATE TABLE encryption_keys (
    id text PRIMARY KEY,
    canary bytea NOT NULL,
    registered_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE signing_secrets (
    client_id uuid NOT NULL,
    destination_id uuid NOT NULL,
    version integer NOT NULL CHECK (version > 0),
    key_id text NOT NULL REFERENCES encryption_keys(id),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) >= 28),
    state text NOT NULL CHECK (state IN ('staged', 'active', 'retired', 'revoked')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (destination_id, version),
    FOREIGN KEY (client_id, destination_id) REFERENCES destinations(client_id, id)
);
CREATE UNIQUE INDEX signing_secrets_one_staged ON signing_secrets(destination_id) WHERE state='staged';
CREATE UNIQUE INDEX signing_secrets_one_active ON signing_secrets(destination_id) WHERE state='active';
