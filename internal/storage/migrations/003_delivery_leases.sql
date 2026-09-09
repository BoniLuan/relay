ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries
    ADD COLUMN lease_token uuid,
    ADD COLUMN lease_owner uuid,
    ADD COLUMN lease_expires_at timestamptz,
    ADD CONSTRAINT deliveries_status_check CHECK (status IN ('pending', 'leased')),
    ADD CONSTRAINT deliveries_lease_check CHECK (
        (status = 'pending' AND lease_token IS NULL AND lease_owner IS NULL AND lease_expires_at IS NULL)
        OR
        (status = 'leased' AND lease_token IS NOT NULL AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
    );
CREATE INDEX deliveries_pending_order ON deliveries(created_at, event_id) WHERE status = 'pending';
CREATE INDEX deliveries_lease_expiry ON deliveries(lease_expires_at, created_at, event_id) WHERE status = 'leased';
