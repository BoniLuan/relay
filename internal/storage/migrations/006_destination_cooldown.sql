-- Preparation failures must not let an unavailable destination monopolize claims.
ALTER TABLE destinations ADD COLUMN delivery_paused_until timestamptz;
