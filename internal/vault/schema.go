package vault

// migrations[i] takes the vault from schema version i to i+1 (PRAGMA
// user_version). Released migrations are never edited: a change of layout
// is a new entry at the end. docs/data-model.md describes the tables.
var migrations = []string{
	// 1: the first layout.
	`
CREATE TABLE sources (
    id       INTEGER PRIMARY KEY,
    platform TEXT NOT NULL,          -- 'wechat'
    account  TEXT NOT NULL,          -- the platform's account directory or ID
    self     TEXT,                   -- the account's own username
    UNIQUE (platform, account)
);

CREATE TABLE runs (
    id           INTEGER PRIMARY KEY,
    source_id    INTEGER NOT NULL REFERENCES sources(id),
    started_at   INTEGER NOT NULL,   -- unix seconds
    finished_at  INTEGER,
    tool_version TEXT,
    added        INTEGER NOT NULL DEFAULT 0,
    revised      INTEGER NOT NULL DEFAULT 0
);

-- Layer 1, imported: only ever added to. A message is first seen once; when
-- the platform later shows it different (recalled, edited, status changed),
-- the new form goes to revisions and this row keeps the original.
CREATE TABLE messages (
    id          INTEGER PRIMARY KEY,
    source_id   INTEGER NOT NULL REFERENCES sources(id),
    chat        TEXT NOT NULL,       -- the conversation's username
    msg_key     TEXT NOT NULL,       -- stable identity, see Message.Key
    server_id   INTEGER,
    local_id    INTEGER,
    sort_seq    INTEGER,
    type        INTEGER NOT NULL,    -- low 32 bits of WeChat's local_type
    subtype     INTEGER NOT NULL,    -- high 32 bits (the appmsg type for 49)
    time        INTEGER NOT NULL,    -- unix seconds
    sender      TEXT,                -- sender username, NULL if unknown
    content     BLOB,                -- as stored by the platform (maybe zstd)
    source_xml  BLOB,
    packed_info BLOB,
    origin      TEXT,                -- the platform file it came from
    hash        BLOB NOT NULL,       -- of type and content, to spot revisions
    run_id      INTEGER NOT NULL REFERENCES runs(id),
    UNIQUE (source_id, chat, msg_key)
);
CREATE INDEX messages_chat_time ON messages (source_id, chat, time);
CREATE INDEX messages_run ON messages (run_id);

CREATE TABLE revisions (
    message_id  INTEGER NOT NULL REFERENCES messages(id),
    run_id      INTEGER NOT NULL REFERENCES runs(id),
    type        INTEGER NOT NULL,
    subtype     INTEGER NOT NULL,
    content     BLOB,
    source_xml  BLOB,
    hash        BLOB NOT NULL,
    PRIMARY KEY (message_id, hash)
);
CREATE INDEX revisions_run ON revisions (run_id);

-- Contacts and group names as last seen; contact_history keeps each
-- distinct earlier form, so renames are not lost.
CREATE TABLE contacts (
    source_id INTEGER NOT NULL REFERENCES sources(id),
    username  TEXT NOT NULL,
    name      TEXT,                  -- remark, else nickname, else alias
    remark    TEXT,
    nickname  TEXT,
    alias     TEXT,
    raw       TEXT NOT NULL,         -- every non-blob column, as JSON
    run_id    INTEGER NOT NULL REFERENCES runs(id),
    PRIMARY KEY (source_id, username)
);
CREATE TABLE contact_history (
    source_id INTEGER NOT NULL,
    username  TEXT NOT NULL,
    run_id    INTEGER NOT NULL,
    raw       TEXT NOT NULL,
    PRIMARY KEY (source_id, username, run_id)
);

-- Layer 2, derived: rebuilt whenever the parser version moves on.
CREATE TABLE chats (
    source_id     INTEGER NOT NULL,
    chat          TEXT NOT NULL,
    kind          TEXT NOT NULL,     -- direct, group, official, openim, other
    first_time    INTEGER,
    last_time     INTEGER,
    message_count INTEGER NOT NULL,
    PRIMARY KEY (source_id, chat)
);
CREATE TABLE parsed (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id),
    parser     INTEGER NOT NULL,     -- parse.Version that wrote the row
    kind       TEXT NOT NULL,        -- text, image, link, file, quote, system, ...
    text       TEXT,                 -- readable form
    data       TEXT,                 -- kind-specific fields, JSON
    sender     TEXT                  -- sender from a group message's text prefix
);
CREATE INDEX parsed_parser ON parsed (parser);

-- Layer 3, the user's own: never written by an import.
CREATE TABLE chat_prefs (
    source_id  INTEGER NOT NULL,
    chat       TEXT NOT NULL,
    alias      TEXT,                 -- overrides the contact name everywhere
    folder     TEXT,                 -- groups chats in exports, e.g. family/
    hidden     INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (source_id, chat)
);

-- Export targets and the files each one wrote, so an export can redo only
-- what changed and remove what it no longer produces.
CREATE TABLE exports (
    name       TEXT PRIMARY KEY,
    format     TEXT NOT NULL,        -- jsonl, md
    dir        TEXT NOT NULL,
    config     TEXT NOT NULL,        -- JSON; a change rebuilds the target
    run_id     INTEGER NOT NULL DEFAULT 0,   -- newest run exported
    parser     INTEGER NOT NULL DEFAULT 0,
    prefs_at   INTEGER NOT NULL DEFAULT 0,   -- newest chat_prefs.updated_at seen
    updated_at INTEGER
);
CREATE TABLE export_files (
    export   TEXT NOT NULL REFERENCES exports(name),
    path     TEXT NOT NULL,          -- relative to the target's dir
    source_id INTEGER NOT NULL,
    chat     TEXT NOT NULL,
    month    TEXT NOT NULL,          -- YYYY-MM
    messages INTEGER NOT NULL,
    PRIMARY KEY (export, path)
);
CREATE INDEX export_files_chat ON export_files (export, source_id, chat, month);
`,
}
