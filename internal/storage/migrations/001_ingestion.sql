CREATE TABLE clients (
    id uuid PRIMARY KEY,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE destinations (
    id uuid PRIMARY KEY,
    client_id uuid NOT NULL REFERENCES clients(id),
    url text NOT NULL CHECK (length(url) BETWEEN 1 AND 2048),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (client_id, id)
);
CREATE TABLE events (
    id uuid PRIMARY KEY,
    client_id uuid NOT NULL REFERENCES clients(id),
    destination_id uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (client_id, idempotency_key),
    FOREIGN KEY (client_id, destination_id) REFERENCES destinations(client_id, id)
);
CREATE TABLE deliveries (
    event_id uuid PRIMARY KEY REFERENCES events(id),
    status text NOT NULL DEFAULT 'pending' CHECK (status = 'pending'),
    created_at timestamptz NOT NULL DEFAULT now()
);
