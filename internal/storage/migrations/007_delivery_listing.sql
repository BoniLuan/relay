-- Support owner-scoped keyset scans, with and without a destination filter.
CREATE INDEX events_client_page ON events(client_id,created_at DESC,id DESC);
CREATE INDEX events_client_destination_page ON events(client_id,destination_id,created_at DESC,id DESC);
