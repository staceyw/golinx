CREATE TABLE IF NOT EXISTS Linx (
    ID              INTEGER PRIMARY KEY AUTOINCREMENT,
    Type            TEXT    NOT NULL DEFAULT 'link',
    ShortName       TEXT    UNIQUE NOT NULL,
    DestinationURL  TEXT    NOT NULL DEFAULT '',
    Description     TEXT    NOT NULL DEFAULT '',
    Owner           TEXT    NOT NULL DEFAULT '',
    LastClicked     INTEGER NOT NULL DEFAULT 0,
    ClickCount      INTEGER NOT NULL DEFAULT 0,
    FirstName       TEXT    NOT NULL DEFAULT '',
    LastName        TEXT    NOT NULL DEFAULT '',
    Title           TEXT    NOT NULL DEFAULT '',
    Email           TEXT    NOT NULL DEFAULT '',
    Phone           TEXT    NOT NULL DEFAULT '',
    WebLink         TEXT    NOT NULL DEFAULT '',
    CalLink         TEXT    NOT NULL DEFAULT '',
    XLink           TEXT    NOT NULL DEFAULT '',
    LinkedInLink    TEXT    NOT NULL DEFAULT '',
    AvatarData      BLOB,
    AvatarMime      TEXT    NOT NULL DEFAULT '',
    DocumentData    BLOB,
    DocumentMime    TEXT    NOT NULL DEFAULT '',
    Color           TEXT    NOT NULL DEFAULT '',
    Tags            TEXT    NOT NULL DEFAULT '',
    DateCreated     INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_linx_shortname_lower ON Linx (LOWER(ShortName));

CREATE TABLE IF NOT EXISTS Settings (
    username TEXT NOT NULL DEFAULT 'default',
    key      TEXT NOT NULL,
    value    TEXT NOT NULL,
    PRIMARY KEY (username, key)
);

CREATE TABLE IF NOT EXISTS ClickLog (
    LinxID    INTEGER NOT NULL,
    ClickedAt INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    FOREIGN KEY (LinxID) REFERENCES Linx(ID) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_clicklog_clickedat ON ClickLog(ClickedAt);
