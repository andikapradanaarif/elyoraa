CREATE TABLE IF NOT EXISTS webhook_events (
    delivery_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    action TEXT,
    owner TEXT NOT NULL,
    repo TEXT NOT NULL,
    pr_number INT,
    payload JSONB,
    status TEXT NOT NULL,
    received_at TIMESTAMPTZ DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    error TEXT
);
