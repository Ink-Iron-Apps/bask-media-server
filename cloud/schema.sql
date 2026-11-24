-- The account service's whole store of record.
--
-- Six tables and deliberately nothing else: the service knows that an account
-- may reach a server, and never what is on it. See docs/CLOUD-ACCOUNTS.md for
-- why that boundary is the point rather than an omission.
--
-- Every secret here is a SHA-256 of something 32 bytes long from the runtime's
-- random source, except a password, which is PBKDF2 with its own salt. Nothing
-- is recoverable from this file.

CREATE TABLE IF NOT EXISTS accounts (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL,
    -- Folded for lookup so two people cannot register the same address in
    -- different cases and then argue about which of them owns it.
    email_folded  TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    password_salt TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

-- One row per signed-in device. The token is the device's own, so revoking it
-- signs out one television and nothing else.
CREATE TABLE IF NOT EXISTS clients (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    label        TEXT NOT NULL DEFAULT '',
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS clients_account ON clients(account_id);

-- A device part way through being linked: the code on the television, and the
-- secret only that television holds.
--
-- Separate from clients because most of these never become one. They are swept
-- on expiry rather than on a timer: the row is in hand whenever it is read.
CREATE TABLE IF NOT EXISTS link_requests (
    device_hash  TEXT PRIMARY KEY,
    user_code    TEXT NOT NULL UNIQUE,
    client_name  TEXT NOT NULL DEFAULT '',
    device_name  TEXT NOT NULL DEFAULT '',
    account_id   TEXT REFERENCES accounts(id) ON DELETE CASCADE,
    approved_at  INTEGER NOT NULL DEFAULT 0,
    expires_at   INTEGER NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS link_expiry ON link_requests(expires_at);

-- A claim token is how a server comes to be owned, and it exists so that the
-- two things that must never meet do not have to: the server never sees an
-- account's credentials, and the owner never hand-carries a server's
-- long-lived secret. The owner mints one while signed in, reads it to the
-- server, and the server redeems it itself. Four minutes and one use, so a
-- token read aloud, pasted into a chat or left in a terminal's scrollback is
-- worth nothing by the time anybody else finds it.
--
-- Only the hash is stored, for the same reason passwords are not stored:
-- a copy of this table is not a set of working claim tokens.
CREATE TABLE IF NOT EXISTS claim_tokens (
    token_hash TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS claim_tokens_expiry ON claim_tokens(expires_at);

CREATE TABLE IF NOT EXISTS servers (
    id          TEXT PRIMARY KEY,
    owner_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    secret_hash TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,
    seen_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS servers_owner ON servers(owner_id);

-- Whatever reaches a server: a tailnet name, a tunnel hostname, an address and
-- a port. The service hands these out and never connects to one, so it never
-- learns whether any of them work — the client finds that out.
CREATE TABLE IF NOT EXISTS addresses (
    server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    address   TEXT NOT NULL,
    position  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (server_id, address)
);

-- An account allowed on a server. Says nothing about what they may watch:
-- that is the server administrator's decision and it is never recorded here.
CREATE TABLE IF NOT EXISTS memberships (
    server_id  TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (server_id, account_id)
);
CREATE INDEX IF NOT EXISTS memberships_account ON memberships(account_id);

CREATE TABLE IF NOT EXISTS invitations (
    code_hash    TEXT PRIMARY KEY,
    server_id    TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    email_folded TEXT NOT NULL,
    expires_at   INTEGER NOT NULL,
    accepted_at  INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS invitations_server ON invitations(server_id);
