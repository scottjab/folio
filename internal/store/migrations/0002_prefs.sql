-- Per-user preferences.
--
-- A key/value table rather than columns on `users` because these are settings,
-- not identity: they arrive one at a time as features land, and every one of
-- them would otherwise be a schema migration plus a new field on the row struct
-- that reads and writes every user. The value is JSON so a setting that grows a
-- second field does not need a third column.
--
-- Only settings that both front ends have to agree on belong here. Anything the
-- browser alone cares about, the text width for instance, stays in that
-- browser's localStorage where it does not cost a round trip.
CREATE TABLE prefs (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key     TEXT    NOT NULL,
    value   TEXT    NOT NULL,
    PRIMARY KEY (user_id, key)
) WITHOUT ROWID;
