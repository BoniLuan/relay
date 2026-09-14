-- One explicit replay may grant three additional starts. Existing terminal work
-- remains terminal; migration alone never authorizes delivery.
ALTER TABLE deliveries ADD COLUMN attempt_limit integer NOT NULL DEFAULT 3
 CHECK (attempt_limit BETWEEN 3 AND 6);
ALTER TABLE deliveries DROP CONSTRAINT deliveries_attempt_count_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_attempt_count_check
 CHECK (attempt_count BETWEEN 0 AND attempt_limit);
ALTER TABLE deliveries DROP CONSTRAINT deliveries_retry_schedule;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_retry_schedule CHECK (
 (status='retry_wait' AND next_attempt_at IS NOT NULL AND attempt_count>=1 AND attempt_count<attempt_limit)
 OR (status<>'retry_wait' AND next_attempt_at IS NULL)
);
ALTER TABLE delivery_attempts DROP CONSTRAINT delivery_attempt_number;
ALTER TABLE delivery_attempts ADD CONSTRAINT delivery_attempt_number
 CHECK (attempt_number BETWEEN 1 AND 6);
CREATE TABLE delivery_replays (
 id uuid PRIMARY KEY,
 event_id uuid NOT NULL UNIQUE REFERENCES deliveries(event_id),
 requested_by uuid NOT NULL REFERENCES clients(id),
 requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 idempotency_hash bytea NOT NULL CHECK (octet_length(idempotency_hash)=32),
 request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
 previous_attempt_count integer NOT NULL CHECK (previous_attempt_count BETWEEN 1 AND 3),
 attempt_limit integer NOT NULL CHECK (attempt_limit=previous_attempt_count+3)
);
