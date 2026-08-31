-- Authoritative tables: users, vaults, shares. These have no markdown home, so
-- they are the only rows in this database that cannot be rebuilt from disk.
CREATE TABLE users (
    id                INTEGER PRIMARY KEY,
    tailscale_user_id INTEGER NOT NULL UNIQUE,
    login             TEXT    NOT NULL,
    display_name      TEXT    NOT NULL DEFAULT '',
    profile_pic_url   TEXT    NOT NULL DEFAULT '',
    -- vault_dir is pinned at first sight and never changes, so renaming your
    -- tailnet login does not orphan your notes.
    vault_dir         TEXT    NOT NULL UNIQUE,
    created_at        INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX users_login ON users (login);

CREATE TABLE vaults (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    dir        TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
CREATE INDEX vaults_user ON vaults (user_id);

CREATE TABLE shares (
    id            TEXT    PRIMARY KEY,
    vault_id      INTEGER NOT NULL REFERENCES vaults (id) ON DELETE CASCADE,
    owner_user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    path          TEXT    NOT NULL,
    is_folder     INTEGER NOT NULL DEFAULT 0,
    grantee_login TEXT    NOT NULL,
    perm          TEXT    NOT NULL CHECK (perm IN ('read', 'write')),
    created_at    INTEGER NOT NULL,
    UNIQUE (vault_id, path, grantee_login)
);
CREATE INDEX shares_grantee ON shares (grantee_login);
CREATE INDEX shares_vault ON shares (vault_id);

-- Derived tables below. `tsnotes index rebuild` truncates and repopulates these
-- from the markdown files, and must never touch anything above.
CREATE TABLE notes (
    id          INTEGER PRIMARY KEY,
    vault_id    INTEGER NOT NULL REFERENCES vaults (id) ON DELETE CASCADE,
    path        TEXT    NOT NULL,
    note_uuid   TEXT    NOT NULL DEFAULT '',
    title       TEXT    NOT NULL DEFAULT '',
    sha256      TEXT    NOT NULL,
    size        INTEGER NOT NULL DEFAULT 0,
    mtime       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0,
    frontmatter TEXT    NOT NULL DEFAULT '',
    UNIQUE (vault_id, path)
);
CREATE INDEX notes_vault_updated ON notes (vault_id, updated_at DESC);
CREATE INDEX notes_uuid ON notes (note_uuid);

-- A plain (not contentless) FTS5 table: it keeps its own copy of the text,
-- which is what makes snippet() and highlight() work. Notes are small; the
-- duplication is worth a highlighted search result. rowid == notes.id.
CREATE VIRTUAL TABLE notes_fts USING fts5 (
    title,
    body,
    tags,
    path,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE tags (
    note_id INTEGER NOT NULL REFERENCES notes (id) ON DELETE CASCADE,
    tag     TEXT    NOT NULL,
    PRIMARY KEY (note_id, tag)
) WITHOUT ROWID;
CREATE INDEX tags_tag ON tags (tag COLLATE NOCASE);

CREATE TABLE links (
    src_note_id INTEGER NOT NULL REFERENCES notes (id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,
    target      TEXT    NOT NULL,
    -- target_path is '' while a link is dangling. It fills in on its own the
    -- next time the missing note is indexed.
    target_path TEXT    NOT NULL DEFAULT '',
    anchor      TEXT    NOT NULL DEFAULT '',
    alias       TEXT    NOT NULL DEFAULT '',
    ord         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX links_src ON links (src_note_id);
CREATE INDEX links_target ON links (target_path);

-- FTS5 is a virtual table, so foreign keys and ON DELETE CASCADE do not reach
-- it. This trigger keeps the search index from accumulating rows for notes that
-- no longer exist. Inserts and updates are done explicitly by the indexer,
-- because the searchable text is parsed from markdown rather than stored in the
-- notes table.
CREATE TRIGGER notes_fts_delete AFTER DELETE ON notes BEGIN
    DELETE FROM notes_fts WHERE rowid = old.id;
END;
