-- One reusable counter per client, never one row per request or bearer token.
CREATE TABLE client_request_limits (
 client_id uuid PRIMARY KEY REFERENCES clients(id),
 window_start timestamptz NOT NULL,
 requests integer NOT NULL CHECK (requests BETWEEN 0 AND 120)
);
