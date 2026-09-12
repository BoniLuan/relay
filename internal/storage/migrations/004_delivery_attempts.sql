-- New events retain exact payload bytes; legacy events use JSONB rendering.
ALTER TABLE events ADD COLUMN payload_bytes bytea CHECK (octet_length(payload_bytes) BETWEEN 1 AND 65536);
ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries DROP CONSTRAINT deliveries_lease_check;
ALTER TABLE deliveries
 ADD CONSTRAINT deliveries_status_check CHECK (status IN ('pending','leased','attempting','succeeded','failed','unknown')),
 ADD CONSTRAINT deliveries_lease_check CHECK (
  (status IN ('pending','succeeded','failed','unknown') AND lease_token IS NULL AND lease_owner IS NULL AND lease_expires_at IS NULL)
  OR (status IN ('leased','attempting') AND lease_token IS NOT NULL AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
 );
CREATE INDEX deliveries_attempt_expiry ON deliveries(lease_expires_at,event_id) WHERE status='attempting';
CREATE TABLE delivery_attempts (
 id uuid PRIMARY KEY,
 event_id uuid NOT NULL REFERENCES deliveries(event_id),
 destination_id uuid NOT NULL,
 signing_version integer NOT NULL,
 lease_token uuid NOT NULL,
 state text NOT NULL CHECK (state IN ('started','succeeded','failed','unknown')),
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 http_status integer CHECK (http_status BETWEEN 100 AND 599),
 error_code text CHECK (error_code IN ('http_status','destination','network','response','input','canceled','interrupted')),
 FOREIGN KEY (destination_id,signing_version) REFERENCES signing_secrets(destination_id,version),
 UNIQUE (event_id,lease_token),
 CHECK (
  (state='started' AND finished_at IS NULL AND http_status IS NULL AND error_code IS NULL)
  OR (state='succeeded' AND finished_at IS NOT NULL AND http_status IS NOT NULL AND http_status BETWEEN 200 AND 299 AND error_code IS NULL)
  OR (state='failed' AND finished_at IS NOT NULL AND error_code IS NOT NULL AND error_code<>'interrupted')
  OR (state='unknown' AND finished_at IS NOT NULL AND http_status IS NULL AND error_code IS NOT NULL AND error_code='interrupted')
 )
);
CREATE UNIQUE INDEX delivery_attempts_one_started ON delivery_attempts(event_id) WHERE state='started';
