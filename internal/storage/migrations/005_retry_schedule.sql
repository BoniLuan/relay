ALTER TABLE deliveries ADD COLUMN attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 3);
ALTER TABLE deliveries ADD COLUMN next_attempt_at timestamptz;
UPDATE deliveries d SET attempt_count=(SELECT count(*) FROM delivery_attempts a WHERE a.event_id=d.event_id);
ALTER TABLE delivery_attempts ADD COLUMN attempt_number integer;
WITH numbered AS (
 SELECT id,row_number() OVER (PARTITION BY event_id ORDER BY started_at,id) AS n FROM delivery_attempts
)
UPDATE delivery_attempts a SET attempt_number=n.n FROM numbered n WHERE a.id=n.id;
ALTER TABLE delivery_attempts ALTER COLUMN attempt_number SET NOT NULL;
ALTER TABLE delivery_attempts ADD CONSTRAINT delivery_attempt_number CHECK (attempt_number BETWEEN 1 AND 3);
ALTER TABLE delivery_attempts ADD CONSTRAINT delivery_attempt_order UNIQUE (event_id,attempt_number);
ALTER TABLE deliveries DROP CONSTRAINT deliveries_status_check;
ALTER TABLE deliveries DROP CONSTRAINT deliveries_lease_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_check
 CHECK (status IN ('pending','leased','attempting','retry_wait','succeeded','failed','unknown'));
ALTER TABLE deliveries ADD CONSTRAINT deliveries_lease_check CHECK (
 (status IN ('pending','retry_wait','succeeded','failed','unknown') AND lease_token IS NULL AND lease_owner IS NULL AND lease_expires_at IS NULL)
 OR (status IN ('leased','attempting') AND lease_token IS NOT NULL AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
);
ALTER TABLE deliveries ADD CONSTRAINT deliveries_retry_schedule CHECK (
 (status='retry_wait' AND next_attempt_at IS NOT NULL AND attempt_count BETWEEN 1 AND 2)
 OR (status<>'retry_wait' AND next_attempt_at IS NULL)
);
CREATE INDEX deliveries_retry_due ON deliveries(next_attempt_at,created_at,event_id) WHERE status='retry_wait';
-- Existing terminal outcomes stay terminal; migration never authorizes a resend.
