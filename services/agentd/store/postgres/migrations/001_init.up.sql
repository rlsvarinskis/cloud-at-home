-- agentd schema. Identifiers are compared in C collation so that ID order (keyset paging, claim order)
-- is byte order, matching the in-memory reference store.

CREATE TABLE agents (
    id              TEXT PRIMARY KEY COLLATE "C",
    project_id      TEXT NOT NULL COLLATE "C",
    name            TEXT NOT NULL,
    vm_id           TEXT NOT NULL,
    model           TEXT NOT NULL,
    system_prompt   TEXT NOT NULL,
    workdir         TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('active', 'orphaned', 'deleting')),
    labels          JSONB NOT NULL,
    idempotency_key TEXT NOT NULL,
    version         BIGINT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (project_id, name)
);

CREATE UNIQUE INDEX agents_idempotency_key_idx ON agents (project_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX agents_vm_id_idx ON agents (vm_id);

-- agent_id deliberately has no foreign key: CreateTask does not require the agent to exist. DeleteAgent
-- removes the agent's tasks itself, in the same transaction.
CREATE TABLE tasks (
    id                TEXT PRIMARY KEY COLLATE "C",
    agent_id          TEXT NOT NULL COLLATE "C",
    project_id        TEXT NOT NULL COLLATE "C",
    prompt            TEXT NOT NULL,
    check_command     TEXT NOT NULL,
    max_steps         INTEGER NOT NULL,
    state             TEXT NOT NULL CHECK (state IN
                          ('queued', 'running', 'succeeded', 'succeeded_unverified', 'failed', 'gave_up',
                           'cancelled', 'error')),
    verdict           TEXT NOT NULL,
    result            TEXT NOT NULL,
    steps             INTEGER NOT NULL,
    attempts          INTEGER NOT NULL,
    cancel_requested  BOOLEAN NOT NULL,
    idempotency_key   TEXT NOT NULL,
    lease_owner       TEXT NOT NULL,
    lease_until       TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    version           BIGINT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    -- run options
    check_each_step   BOOLEAN NOT NULL DEFAULT TRUE,
    max_minutes       INTEGER NOT NULL DEFAULT 0,
    max_call_tokens   INTEGER NOT NULL DEFAULT 32768,
    -- how the task ended, and who decided its state ('check' or 'model')
    reason            TEXT NOT NULL DEFAULT '',
    verdict_source    TEXT NOT NULL DEFAULT '',
    -- token and timing counters, summed over every model call (aborted calls included)
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    model_calls       INTEGER NOT NULL DEFAULT 0,
    partial_calls     INTEGER NOT NULL DEFAULT 0,
    first_token_seconds   DOUBLE PRECISION NOT NULL DEFAULT 0,
    first_command_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- the model call in progress, and the most recent call records
    live_answer       TEXT NOT NULL DEFAULT '',
    live_reasoning    TEXT NOT NULL DEFAULT '',
    calls             JSONB NOT NULL DEFAULT '[]'
);

CREATE UNIQUE INDEX tasks_idempotency_key_idx ON tasks (project_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX tasks_claim_idx ON tasks (state, id) WHERE state IN ('queued', 'running');
CREATE INDEX tasks_agent_id_idx ON tasks (agent_id, id);
CREATE INDEX tasks_project_id_idx ON tasks (project_id, id);

CREATE TABLE steps (
    task_id   TEXT NOT NULL COLLATE "C" REFERENCES tasks (id) ON DELETE CASCADE,
    seq       INTEGER NOT NULL,
    command   TEXT NOT NULL,
    output    TEXT NOT NULL,
    exit_code INTEGER,
    at        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (task_id, seq)
);

CREATE TABLE messages (
    task_id TEXT NOT NULL COLLATE "C" REFERENCES tasks (id) ON DELETE CASCADE,
    seq     INTEGER NOT NULL,
    json    BYTEA NOT NULL,
    at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (task_id, seq)
);
