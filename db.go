package main

import (
	"database/sql"
	_ "embed"
	"errors"
	"io/fs"
	"sync"
	"time"
)

//go:embed schema.sql
var sqlSchema string

// Linx type constants.
const (
	LinxTypeLink     = "link"
	LinxTypeEmployee = "employee"
	LinxTypeCustomer = "customer"
	LinxTypeVendor   = "vendor"
	LinxTypeDocument = "document"
)

// Linx represents any item in GoLinx: a link, person, or document.
type Linx struct {
	ID             int64  `json:"id"`
	Type           string `json:"type"`
	ShortName      string `json:"shortName"`
	DestinationURL string `json:"destinationURL"`
	Description    string `json:"description"`
	Owner          string `json:"owner"`
	LastClicked    int64  `json:"lastClicked"`
	ClickCount     int64  `json:"clickCount"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Title        string `json:"title"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	WebLink      string `json:"webLink"`
	CalLink      string `json:"calLink"`
	XLink        string `json:"xLink"`
	LinkedInLink string `json:"linkedInLink"`
	AvatarMime     string `json:"avatarMime,omitempty"`
	DocumentMime   string `json:"documentMime,omitempty"`
	Color          string `json:"color"`
	Tags           string `json:"tags"`
	DateCreated    int64  `json:"dateCreated"`
	DeletedAt      int64  `json:"deletedAt,omitempty"`
}

// IsPersonType returns true for linx types that use the person form and profile page.
func (c *Linx) IsPersonType() bool {
	return c.Type == LinxTypeEmployee || c.Type == LinxTypeCustomer || c.Type == LinxTypeVendor
}

// IsDocumentType returns true for the document linx type.
func (c *Linx) IsDocumentType() bool {
	return c.Type == LinxTypeDocument
}

const linxColumns = `ID, Type, ShortName, DestinationURL, Description, Owner, LastClicked, ClickCount, FirstName, LastName, Title, Email, Phone, WebLink, CalLink, XLink, LinkedInLink, AvatarMime, DocumentMime, Color, Tags, DateCreated, DeletedAt`

func scanLinx(scanner interface{ Scan(dest ...any) error }) (*Linx, error) {
	c := new(Linx)
	err := scanner.Scan(&c.ID, &c.Type, &c.ShortName, &c.DestinationURL,
		&c.Description, &c.Owner, &c.LastClicked, &c.ClickCount,
		&c.FirstName, &c.LastName, &c.Title, &c.Email, &c.Phone,
		&c.WebLink, &c.CalLink, &c.XLink, &c.LinkedInLink, &c.AvatarMime, &c.DocumentMime, &c.Color, &c.Tags, &c.DateCreated, &c.DeletedAt)
	return c, err
}

// SQLiteDB wraps the database connection with a mutex for safe concurrent access.
type SQLiteDB struct {
	db *sql.DB
	mu sync.RWMutex
}

// NewSQLiteDB opens a SQLite database and initializes the schema.
func NewSQLiteDB(f string) (*SQLiteDB, error) {
	db, err := sql.Open("sqlite", f)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, err
		}
	}
	// Migrate: rename Cards → Linx if the old table name exists.
	var oldTable string
	_ = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='Cards'").Scan(&oldTable)
	if oldTable == "Cards" {
		db.Exec("ALTER TABLE Cards RENAME TO Linx")
		db.Exec("DROP INDEX IF EXISTS idx_cards_shortname_lower")
	}
	if _, err = db.Exec(sqlSchema); err != nil {
		return nil, err
	}
	// Migrate: add columns that may not exist in older databases.
	for _, col := range []string{
		"ALTER TABLE Linx ADD COLUMN Color TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE Linx ADD COLUMN Tags TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE Linx ADD COLUMN DeletedAt INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE Linx ADD COLUMN DocumentData BLOB",
		"ALTER TABLE Linx ADD COLUMN DocumentMime TEXT NOT NULL DEFAULT ''",
	} {
		db.Exec(col) // ignore "duplicate column" errors
	}
	return &SQLiteDB{db: db}, nil
}

// LoadAll returns all linx, optionally filtered by type, ordered by ShortName.
func (s *SQLiteDB) LoadAll(filterType string) ([]*Linx, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := "SELECT " + linxColumns + " FROM Linx WHERE DeletedAt = 0"
	var args []any
	if filterType != "" {
		query += " AND Type = ?"
		args = append(args, filterType)
	}
	query += " ORDER BY LOWER(ShortName)"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*Linx
	for rows.Next() {
		c, err := scanLinx(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

// LoadByID returns a single linx by ID.
func (s *SQLiteDB) LoadByID(id int64) (*Linx, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	c, err := scanLinx(s.db.QueryRow("SELECT "+linxColumns+" FROM Linx WHERE ID = ?", id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return c, nil
}

// LoadByShortName returns a single linx by short name (case-insensitive).
func (s *SQLiteDB) LoadByShortName(name string) (*Linx, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	c, err := scanLinx(s.db.QueryRow("SELECT "+linxColumns+" FROM Linx WHERE LOWER(ShortName) = LOWER(?) AND DeletedAt = 0", name))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	return c, nil
}

// Save inserts a new linx and returns its ID.
func (s *SQLiteDB) Save(lnx *Linx) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertLinx(lnx)
}

func (s *SQLiteDB) insertLinx(lnx *Linx) (int64, error) {
	if lnx.Type == "" {
		lnx.Type = LinxTypeLink
	}
	// Free up the short name if occupied by a soft-deleted item.
	s.db.Exec("DELETE FROM Linx WHERE LOWER(ShortName) = LOWER(?) AND DeletedAt > 0", lnx.ShortName)
	now := time.Now().Unix()
	result, err := s.db.Exec(
		`INSERT INTO Linx (Type, ShortName, DestinationURL, Description, Owner, FirstName, LastName, Title, Email, Phone, WebLink, CalLink, XLink, LinkedInLink, Color, Tags, DateCreated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lnx.Type, lnx.ShortName, lnx.DestinationURL, lnx.Description, lnx.Owner,
		lnx.FirstName, lnx.LastName, lnx.Title, lnx.Email, lnx.Phone,
		lnx.WebLink, lnx.CalLink, lnx.XLink, lnx.LinkedInLink, lnx.Color, lnx.Tags, now,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// Update modifies an existing linx by ID.
func (s *SQLiteDB) Update(lnx *Linx) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(
		`UPDATE Linx SET Type=?, ShortName=?, DestinationURL=?, Description=?, Owner=?, FirstName=?, LastName=?, Title=?, Email=?, Phone=?, WebLink=?, CalLink=?, XLink=?, LinkedInLink=?, Color=?, Tags=? WHERE ID=?`,
		lnx.Type, lnx.ShortName, lnx.DestinationURL, lnx.Description, lnx.Owner,
		lnx.FirstName, lnx.LastName, lnx.Title, lnx.Email, lnx.Phone,
		lnx.WebLink, lnx.CalLink, lnx.XLink, lnx.LinkedInLink, lnx.Color, lnx.Tags, lnx.ID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fs.ErrNotExist
	}
	return nil
}

// Delete soft-deletes a linx by setting its DeletedAt timestamp.
func (s *SQLiteDB) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	result, err := s.db.Exec("UPDATE Linx SET DeletedAt = ? WHERE ID = ? AND DeletedAt = 0", now, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fs.ErrNotExist
	}
	return nil
}

// Restore un-deletes a soft-deleted linx by clearing its DeletedAt timestamp.
func (s *SQLiteDB) Restore(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("UPDATE Linx SET DeletedAt = 0 WHERE ID = ? AND DeletedAt > 0", id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fs.ErrNotExist
	}
	return nil
}

// LoadDeleted returns all soft-deleted linx, ordered by deletion time (most recent first).
func (s *SQLiteDB) LoadDeleted() ([]*Linx, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT " + linxColumns + " FROM Linx WHERE DeletedAt > 0 ORDER BY DeletedAt DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*Linx
	for rows.Next() {
		c, err := scanLinx(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

// PurgeDeleted permanently removes soft-deleted linx older than the cutoff timestamp.
func (s *SQLiteDB) PurgeDeleted(cutoff int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM Linx WHERE DeletedAt > 0 AND DeletedAt < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// IncrementClick atomically increments click count, updates LastClicked, and logs the click.
func (s *SQLiteDB) IncrementClick(shortName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	_, err := s.db.Exec("UPDATE Linx SET ClickCount = ClickCount + 1, LastClicked = ? WHERE LOWER(ShortName) = LOWER(?) AND DeletedAt = 0", now, shortName)
	if err != nil {
		return err
	}
	// Best-effort click log for analytics.
	s.db.Exec("INSERT INTO ClickLog (LinxID, ClickedAt) SELECT ID, ? FROM Linx WHERE LOWER(ShortName) = LOWER(?) AND DeletedAt = 0", now, shortName)
	return nil
}

// TopLink holds a short name and its click count for stats.
type TopLink struct {
	ShortName  string `json:"shortName"`
	ClickCount int64  `json:"clickCount"`
}

// DailyCount holds a date string and click count for the daily histogram.
type DailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// StatsSummary holds aggregate statistics.
type StatsSummary struct {
	TotalLinks      int    `json:"totalLinks"`
	TotalClicks     int64  `json:"totalClicks"`
	CreatedThisWeek int    `json:"createdThisWeek"`
	TopLink         string `json:"topLink"`
}

// StatsTopLinks returns the top N links by click count.
func (s *SQLiteDB) StatsTopLinks(limit int) ([]TopLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT ShortName, ClickCount FROM Linx WHERE Type = 'link' AND ClickCount > 0 AND DeletedAt = 0 ORDER BY ClickCount DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []TopLink
	for rows.Next() {
		var t TopLink
		if err := rows.Scan(&t.ShortName, &t.ClickCount); err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}

// StatsDailyClicks returns click counts grouped by day for the last N days.
func (s *SQLiteDB) StatsDailyClicks(days int) ([]DailyCount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	since := time.Now().AddDate(0, 0, -days).Unix()
	rows, err := s.db.Query(`SELECT strftime('%Y-%m-%d', ClickedAt, 'unixepoch') AS day, COUNT(*) AS count FROM ClickLog WHERE ClickedAt >= ? GROUP BY day ORDER BY day`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []DailyCount
	for rows.Next() {
		var d DailyCount
		if err := rows.Scan(&d.Date, &d.Count); err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

// LinkDailyClicks returns click counts grouped by day for a specific link over the last N days.
func (s *SQLiteDB) LinkDailyClicks(linxID int64, days int) ([]DailyCount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	since := time.Now().AddDate(0, 0, -days).Unix()
	rows, err := s.db.Query(`SELECT strftime('%Y-%m-%d', ClickedAt, 'unixepoch') AS day, COUNT(*) AS count FROM ClickLog WHERE LinxID = ? AND ClickedAt >= ? GROUP BY day ORDER BY day`, linxID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []DailyCount
	for rows.Next() {
		var d DailyCount
		if err := rows.Scan(&d.Date, &d.Count); err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

// GetStatsSummary returns aggregate statistics.
func (s *SQLiteDB) GetStatsSummary() (*StatsSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	weekAgo := time.Now().AddDate(0, 0, -7).Unix()
	var sum StatsSummary
	var topLink sql.NullString
	err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM Linx WHERE DeletedAt = 0), (SELECT COALESCE(SUM(ClickCount), 0) FROM Linx WHERE DeletedAt = 0), (SELECT COUNT(*) FROM Linx WHERE DeletedAt = 0 AND DateCreated >= ?), (SELECT ShortName FROM Linx WHERE DeletedAt = 0 AND ClickCount > 0 ORDER BY ClickCount DESC LIMIT 1)`, weekAgo).Scan(&sum.TotalLinks, &sum.TotalClicks, &sum.CreatedThisWeek, &topLink)
	if err != nil {
		return nil, err
	}
	sum.TopLink = topLink.String
	return &sum, nil
}

// GetSetting retrieves a setting value.
func (s *SQLiteDB) GetSetting(username, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var value string
	err := s.db.QueryRow("SELECT value FROM Settings WHERE username = ? AND key = ?", username, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// PutSetting saves a setting value.
func (s *SQLiteDB) PutSetting(username, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("INSERT OR REPLACE INTO Settings (username, key, value) VALUES (?, ?, ?)", username, key, value)
	return err
}

// LinxCount returns the total number of linx, optionally filtered by type.
func (s *SQLiteDB) LinxCount(filterType string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := "SELECT COUNT(*) FROM Linx WHERE DeletedAt = 0"
	var args []any
	if filterType != "" {
		query += " AND Type = ?"
		args = append(args, filterType)
	}
	var count int
	err := s.db.QueryRow(query, args...).Scan(&count)
	return count, err
}

// Suggest returns linx whose ShortName or Description contains the query substring,
// ordered by click count (most popular first), limited to n results.
func (s *SQLiteDB) Suggest(query string, limit int) ([]*Linx, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	like := "%" + query + "%"
	rows, err := s.db.Query(
		"SELECT "+linxColumns+" FROM Linx WHERE DeletedAt = 0 AND (LOWER(ShortName) LIKE LOWER(?) OR LOWER(Description) LIKE LOWER(?)) ORDER BY ClickCount DESC LIMIT ?",
		like, like, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*Linx
	for rows.Next() {
		c, err := scanLinx(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

// SaveAvatar updates the avatar for a linx.
func (s *SQLiteDB) SaveAvatar(id int64, data []byte, mime string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("UPDATE Linx SET AvatarData = ?, AvatarMime = ? WHERE ID = ?", data, mime, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fs.ErrNotExist
	}
	return nil
}

// LoadAvatar returns the avatar data and MIME type for a linx.
func (s *SQLiteDB) LoadAvatar(id int64) ([]byte, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var data []byte
	var mime string
	err := s.db.QueryRow("SELECT AvatarData, AvatarMime FROM Linx WHERE ID = ?", id).Scan(&data, &mime)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", fs.ErrNotExist
		}
		return nil, "", err
	}
	return data, mime, nil
}

// SaveDocument updates the document content for a linx.
func (s *SQLiteDB) SaveDocument(id int64, data []byte, mime string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("UPDATE Linx SET DocumentData = ?, DocumentMime = ? WHERE ID = ?", data, mime, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fs.ErrNotExist
	}
	return nil
}

// LoadDocument returns the document content and MIME type for a linx.
func (s *SQLiteDB) LoadDocument(id int64) ([]byte, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var data []byte
	var mime string
	err := s.db.QueryRow("SELECT DocumentData, DocumentMime FROM Linx WHERE ID = ?", id).Scan(&data, &mime)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", fs.ErrNotExist
		}
		return nil, "", err
	}
	return data, mime, nil
}
