-- Multi-instance job infrastructure (QUEUE_BACKEND=postgres).

-- Durable queue. Workers claim rows with FOR UPDATE SKIP LOCKED and hold a
-- lease while processing; a crashed worker's row returns to 'queued' when
-- its lease expires. Rows are deleted on MarkDone (terminal state lives in
-- job_records).
CREATE TABLE IF NOT EXISTS job_queue (
    job_key     TEXT PRIMARY KEY,
    payload     JSONB NOT NULL,
    status      TEXT NOT NULL DEFAULT 'queued',  -- queued | running
    locked_by   TEXT,                            -- claiming instance id
    lease_until TIMESTAMPTZ,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS job_queue_queued ON job_queue (enqueued_at) WHERE status = 'queued';
CREATE INDEX IF NOT EXISTS job_queue_lease ON job_queue (lease_until) WHERE status = 'running';

-- Job outcome records: audit log, /jobs source, retry source, calibration
-- samples, and rate-cap evidence. The job payload is kept for manual retry.
CREATE TABLE IF NOT EXISTS job_records (
    job_key         TEXT PRIMARY KEY,
    delivery_id     TEXT,
    job_type        TEXT,
    owner           TEXT NOT NULL,
    repo            TEXT NOT NULL,
    pr_number       INT,
    status          TEXT NOT NULL,
    error           TEXT,
    attempts        INT NOT NULL DEFAULT 0,
    actions         JSONB,
    refined_lines   INT,
    attempt_seconds BIGINT,
    job             JSONB,
    instance        TEXT,
    enqueued_at     TIMESTAMPTZ,
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS job_records_updated ON job_records (updated_at DESC);
CREATE INDEX IF NOT EXISTS job_records_repo_started ON job_records (owner, repo, started_at);

-- Cross-instance in-flight claims for dedup/supersede. Cancellation travels
-- through the superseded flag: the owning replica's watcher observes it (or
-- sees its claim replaced by another delivery) and cancels the local job
-- context. A claim's payload carries the fields supersession needs
-- (type/origin/delivery) plus pending_comment_id for handoff.
CREATE TABLE IF NOT EXISTS job_claims (
    job_key            TEXT PRIMARY KEY,
    owner              TEXT NOT NULL,
    repo               TEXT NOT NULL,
    pr_number          INT NOT NULL,
    job_type           TEXT,
    delivery_id        TEXT,
    origin             TEXT,
    payload            JSONB,
    pending_comment_id BIGINT,
    instance           TEXT NOT NULL,
    superseded         BOOLEAN NOT NULL DEFAULT false,
    superseded_at      TIMESTAMPTZ,
    claimed_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS job_claims_pr ON job_claims (owner, repo, pr_number);
CREATE INDEX IF NOT EXISTS job_claims_instance ON job_claims (instance);
CREATE INDEX IF NOT EXISTS job_claims_stale ON job_claims (superseded_at) WHERE superseded;
