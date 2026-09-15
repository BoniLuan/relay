-- Existing terminal rows receive a full grace period from migration time.
ALTER TABLE deliveries ADD COLUMN terminal_at timestamptz;
UPDATE deliveries SET terminal_at=statement_timestamp()
 WHERE status IN ('succeeded','failed','unknown');

-- Capture every terminal transition, including crash recovery; reopening via
-- replay clears the clock. No application path can forget to maintain it.
CREATE FUNCTION relay_delivery_terminal_clock() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.status IN ('succeeded','failed','unknown') THEN
  IF TG_OP='INSERT' THEN
   NEW.terminal_at := clock_timestamp();
  ELSIF OLD.status IS DISTINCT FROM NEW.status THEN
   NEW.terminal_at := clock_timestamp();
  END IF;
 ELSE
  NEW.terminal_at := NULL;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER deliveries_terminal_clock BEFORE INSERT OR UPDATE OF status
 ON deliveries FOR EACH ROW EXECUTE FUNCTION relay_delivery_terminal_clock();
ALTER TABLE deliveries ADD CONSTRAINT deliveries_terminal_clock_check CHECK (
 (status IN ('succeeded','failed','unknown') AND terminal_at IS NOT NULL)
 OR (status IN ('pending','leased','attempting','retry_wait') AND terminal_at IS NULL)
);
CREATE INDEX deliveries_retention ON deliveries(terminal_at,event_id)
 WHERE status IN ('succeeded','failed','unknown');
