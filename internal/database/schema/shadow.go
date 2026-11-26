package schema

const ShadowTrial = `
CREATE TABLE IF NOT EXISTS shadow_trial (
    singleton INTEGER PRIMARY KEY CHECK (singleton=1),
    id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('legacy','ledger')),
    seeded INTEGER NOT NULL DEFAULT 0,
    sequence INTEGER NOT NULL DEFAULT 0
);
`

const ShadowQueue = `
CREATE TABLE IF NOT EXISTS shadow_queue (
    sequence INTEGER PRIMARY KEY,
    command_id TEXT NOT NULL,
    author TEXT NOT NULL,
    locale TEXT NOT NULL,
    command BLOB NOT NULL
);
`
