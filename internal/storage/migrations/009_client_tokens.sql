-- Preserve existing bearer credentials while moving their digests into lifecycle
-- records. The initial token's metadata ID is its client's ID (not a credential).
CREATE TABLE client_tokens (
 id uuid PRIMARY KEY,
 client_id uuid NOT NULL REFERENCES clients(id),
 token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
 slot smallint CHECK (slot IN (1,2)),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 revoked_at timestamptz,
 UNIQUE (client_id,slot),
 CHECK ((revoked_at IS NULL AND slot IS NOT NULL)
     OR (revoked_at IS NOT NULL AND slot IS NULL))
);
INSERT INTO client_tokens(id,client_id,token_hash,slot,created_at)
 SELECT id,id,token_hash,1,created_at FROM clients;
ALTER TABLE clients DROP COLUMN token_hash;
CREATE INDEX client_tokens_metadata ON client_tokens(client_id,created_at DESC,id DESC);
