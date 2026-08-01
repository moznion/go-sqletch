CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    email      TEXT NOT NULL,
    status     TEXT NOT NULL,
    tenant_id  INTEGER NOT NULL,
    nickname   TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
