-- Initial vmd schema. Columns mirror the structs in package domain.
--
-- IDs are ULIDs (or similar) that are compared and ordered as bytes, so their columns use the "C"
-- collation: ORDER BY id and "id > $1" then agree with Go's string ordering.
--
-- Empty strings are used for "unset" text fields (as in domain), so those columns are NOT NULL.
-- Only the timestamps and the array/JSON fields that can be unset are nullable.

CREATE TABLE nodes (
    id               text COLLATE "C" PRIMARY KEY,
    address          text NOT NULL,
    state            text NOT NULL,
    cpus             integer NOT NULL,
    memory_mb        bigint NOT NULL,
    disk_bytes_total bigint NOT NULL,
    disk_bytes_free  bigint NOT NULL,
    last_heartbeat   timestamptz NOT NULL
);

CREATE TABLE images (
    id             text COLLATE "C" PRIMARY KEY,
    project_id     text NOT NULL, -- '' means shared by every project
    name           text NOT NULL,
    node_id        text NOT NULL,
    state          text NOT NULL,
    state_reason   text NOT NULL,
    size_bytes     bigint NOT NULL,
    source_disk_id text NOT NULL,
    path           text NOT NULL,
    version        bigint NOT NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (project_id, name)
);

CREATE INDEX images_state_idx ON images (state);

CREATE TABLE disks (
    id             text COLLATE "C" PRIMARY KEY,
    project_id     text NOT NULL,
    name           text NOT NULL,
    node_id        text NOT NULL,
    size_gb        integer NOT NULL,
    image_id       text NOT NULL, -- '' for a blank disk
    state          text NOT NULL,
    state_reason   text NOT NULL,
    attached_vm_id text NOT NULL,
    version        bigint NOT NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (project_id, name)
);

CREATE INDEX disks_state_idx ON disks (state);
CREATE INDEX disks_image_id_idx ON disks (image_id);

CREATE TABLE vms (
    id              text COLLATE "C" PRIMARY KEY,
    project_id      text NOT NULL,
    name            text NOT NULL,
    node_id         text NOT NULL,
    cpus            integer NOT NULL,
    memory_mb       bigint NOT NULL,
    disk_ids        text[],          -- NULL when nil, '{}' when empty
    desired         text NOT NULL,
    state           text NOT NULL,
    state_reason    text NOT NULL,
    holds           text[],          -- NULL when nil, '{}' when empty
    labels          jsonb NOT NULL DEFAULT '{}', -- JSON null for a nil map, {} for an empty one
    generation      bigint NOT NULL,
    version         bigint NOT NULL,
    stop_force      boolean NOT NULL,
    delete_disks    boolean NOT NULL,
    idempotency_key text NOT NULL,
    next_reconcile  timestamptz,     -- NULL = nothing to do
    lease_owner     text NOT NULL,
    lease_until     timestamptz NOT NULL DEFAULT 'epoch'::timestamptz,
    failures        integer NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (project_id, name)
);

CREATE UNIQUE INDEX vms_idempotency_key_idx ON vms (project_id, idempotency_key) WHERE idempotency_key <> '';
CREATE INDEX vms_node_id_idx ON vms (node_id);
CREATE INDEX vms_next_reconcile_idx ON vms (next_reconcile) WHERE next_reconcile IS NOT NULL;

CREATE TABLE jobs (
    id              text COLLATE "C" PRIMARY KEY,
    vm_id           text NOT NULL,
    project_id      text NOT NULL,
    command         text NOT NULL,
    workdir         text NOT NULL,
    state           text NOT NULL,
    exit_code       integer,         -- NULL until the job has exited
    timeout_seconds integer NOT NULL,
    killed_reason   text NOT NULL,
    idempotency_key text NOT NULL,
    version         bigint NOT NULL,
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz      -- NULL while running
);

CREATE UNIQUE INDEX jobs_idempotency_key_idx ON jobs (project_id, idempotency_key) WHERE idempotency_key <> '';
CREATE INDEX jobs_vm_id_id_idx ON jobs (vm_id, id);
CREATE INDEX jobs_running_idx ON jobs (vm_id) WHERE state = 'running';
