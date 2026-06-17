// Package store is the SQLite index/history/log layer. The vault stays the
// source of truth; this just makes the feed + logs queryable and holds the
// editable sources/rules + settings. Pure-Go driver (modernc.org/sqlite) so
// CGO_ENABLED=0 and the image can be distroless/scratch.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

// Open opens (creating if needed) the DB, runs migrations, and seeds defaults.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.seed(); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ── migrations ────────────────────────────────────────────────
// Append-only. Each string is one version; index+1 = version number.
var migrations = []string{
	`CREATE TABLE sources (
		id        INTEGER PRIMARY KEY,
		name      TEXT NOT NULL,
		domain    TEXT NOT NULL UNIQUE,
		enabled   INTEGER NOT NULL DEFAULT 1,
		strategy  TEXT NOT NULL DEFAULT 'rules'
	);
	CREATE TABLE rules (
		id        INTEGER PRIMARY KEY,
		source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		field     TEXT NOT NULL,
		selector  TEXT NOT NULL DEFAULT '',
		regex     TEXT NOT NULL DEFAULT '',
		attr      TEXT NOT NULL DEFAULT '',
		UNIQUE(source_id, field)
	);
	CREATE TABLE books (
		id              INTEGER PRIMARY KEY,
		title           TEXT NOT NULL,
		link            TEXT NOT NULL UNIQUE,
		path            TEXT NOT NULL DEFAULT '',
		volumes         INTEGER NOT NULL DEFAULT 0,
		source_id       INTEGER REFERENCES sources(id) ON DELETE SET NULL,
		cover           TEXT NOT NULL DEFAULT '',
		created_at      TEXT NOT NULL,
		updated_at      TEXT NOT NULL,
		last_checked_at TEXT
	);
	CREATE TABLE updates (
		id          INTEGER PRIMARY KEY,
		book_id     INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
		old_volumes INTEGER NOT NULL,
		new_volumes INTEGER NOT NULL,
		link        TEXT NOT NULL,
		detected_at TEXT NOT NULL
	);
	CREATE TABLE runs (
		id          INTEGER PRIMARY KEY,
		started_at  TEXT NOT NULL,
		finished_at TEXT,
		checked     INTEGER NOT NULL DEFAULT 0,
		updated     INTEGER NOT NULL DEFAULT 0,
		errors      INTEGER NOT NULL DEFAULT 0,
		summary     TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	row := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("v%d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES(?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ── seed: jnovels source + default rules (ported from old Python) ──
func (s *Store) seed() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	res, err := s.db.Exec(
		`INSERT INTO sources(name, domain, enabled, strategy) VALUES(?,?,1,'rules')`,
		"jnovels", "jnovels.com")
	if err != nil {
		return err
	}
	srcID, _ := res.LastInsertId()

	defaults := []struct{ field, selector, regex, attr string }{
		{"volume_list", "ol", "", ""}, // last match used in code
		{"volume_item", "li", `(?i)VOLUME\s*(\d+)`, ""},
		{"title", "h1.post-title.entry-title", "", ""},
		{"cover", "div.featured-media img", "", "src"},
		{"description", "div.synopsis-description || #editdescription", "", ""}, // ordered fallback
	}
	for _, d := range defaults {
		if _, err := s.db.Exec(
			`INSERT INTO rules(source_id, field, selector, regex, attr) VALUES(?,?,?,?,?)`,
			srcID, d.field, d.selector, d.regex, d.attr); err != nil {
			return err
		}
	}
	return nil
}

// ── run + update + book recording (used by the checker/CLI) ──
func now() string { return time.Now().UTC().Format(time.RFC3339) }

// StartRun inserts a runs row and returns its id.
func (s *Store) StartRun() (int64, error) {
	res, err := s.db.Exec(`INSERT INTO runs(started_at) VALUES(?)`, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun stamps the run with its outcome.
func (s *Store) FinishRun(id int64, checked, updated, errors int, summary string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET finished_at=?, checked=?, updated=?, errors=?, summary=? WHERE id=?`,
		now(), checked, updated, errors, summary, id)
	return err
}

// UpsertBook inserts or updates a book by link, returning its id.
func (s *Store) UpsertBook(title, link, path string, volumes int) (int64, error) {
	ts := now()
	_, err := s.db.Exec(`
		INSERT INTO books(title, link, path, volumes, created_at, updated_at, last_checked_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(link) DO UPDATE SET
			title=excluded.title, path=excluded.path,
			volumes=excluded.volumes, updated_at=excluded.updated_at,
			last_checked_at=excluded.last_checked_at`,
		title, link, path, volumes, ts, ts, ts)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(`SELECT id FROM books WHERE link=?`, link).Scan(&id)
	return id, err
}

// ── read models + queries (for the HTTP API) ──────────────────

type Book struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Link          string `json:"link"`
	Path          string `json:"path"`
	Volumes       int    `json:"volumes"`
	UpdatedAt     string `json:"updated_at"`
	LastCheckedAt string `json:"last_checked_at"`
}

func (s *Store) ListBooks() ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, title, link, path, volumes, updated_at,
		COALESCE(last_checked_at,'') FROM books ORDER BY title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Title, &b.Link, &b.Path, &b.Volumes, &b.UpdatedAt, &b.LastCheckedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type Update struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	OldVolumes int    `json:"old_volumes"`
	NewVolumes int    `json:"new_volumes"`
	Link       string `json:"link"`
	DetectedAt string `json:"detected_at"`
}

func (s *Store) ListUpdates(limit int) ([]Update, error) {
	rows, err := s.db.Query(`SELECT u.id, b.title, u.old_volumes, u.new_volumes, u.link, u.detected_at
		FROM updates u JOIN books b ON b.id = u.book_id
		ORDER BY u.detected_at DESC, u.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		var u Update
		if err := rows.Scan(&u.ID, &u.Title, &u.OldVolumes, &u.NewVolumes, &u.Link, &u.DetectedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

type Run struct {
	ID         int64  `json:"id"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Checked    int    `json:"checked"`
	Updated    int    `json:"updated"`
	Errors     int    `json:"errors"`
	Summary    string `json:"summary"`
}

func (s *Store) ListRuns(limit int) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id, started_at, COALESCE(finished_at,''), checked, updated, errors, summary
		FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Checked, &r.Updated, &r.Errors, &r.Summary); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── sources + rules ───────────────────────────────────────────

type Rule struct {
	Field    string `json:"field"`
	Selector string `json:"selector"`
	Regex    string `json:"regex"`
	Attr     string `json:"attr"`
}

type Source struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	Enabled  bool   `json:"enabled"`
	Strategy string `json:"strategy"`
	Rules    []Rule `json:"rules"`
}

func (s *Store) ListSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT id, name, domain, enabled, strategy FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var srcs []Source
	for rows.Next() {
		var src Source
		var en int
		if err := rows.Scan(&src.ID, &src.Name, &src.Domain, &en, &src.Strategy); err != nil {
			return nil, err
		}
		src.Enabled = en != 0
		srcs = append(srcs, src)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range srcs {
		rr, err := s.db.Query(`SELECT field, selector, regex, attr FROM rules WHERE source_id=? ORDER BY field`, srcs[i].ID)
		if err != nil {
			return nil, err
		}
		for rr.Next() {
			var r Rule
			if err := rr.Scan(&r.Field, &r.Selector, &r.Regex, &r.Attr); err != nil {
				rr.Close()
				return nil, err
			}
			srcs[i].Rules = append(srcs[i].Rules, r)
		}
		rr.Close()
	}
	return srcs, nil
}

// UpsertSource inserts or updates a source by domain, returning its id.
func (s *Store) UpsertSource(name, domain, strategy string, enabled bool) (int64, error) {
	en := 0
	if enabled {
		en = 1
	}
	if strategy == "" {
		strategy = "rules"
	}
	_, err := s.db.Exec(`
		INSERT INTO sources(name, domain, enabled, strategy) VALUES(?,?,?,?)
		ON CONFLICT(domain) DO UPDATE SET name=excluded.name, enabled=excluded.enabled, strategy=excluded.strategy`,
		name, domain, en, strategy)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(`SELECT id FROM sources WHERE domain=?`, domain).Scan(&id)
	return id, err
}

func (s *Store) DeleteSource(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE id=?`, id)
	return err
}

// UpsertRule sets one field rule for a source.
func (s *Store) UpsertRule(sourceID int64, field, selector, regex, attr string) error {
	_, err := s.db.Exec(`
		INSERT INTO rules(source_id, field, selector, regex, attr) VALUES(?,?,?,?,?)
		ON CONFLICT(source_id, field) DO UPDATE SET
			selector=excluded.selector, regex=excluded.regex, attr=excluded.attr`,
		sourceID, field, selector, regex, attr)
	return err
}

// ── settings (key/value) ──────────────────────────────────────

func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// BookExists reports whether a book with this link is already tracked.
func (s *Store) BookExists(link string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM books WHERE link=?`, link).Scan(&n)
	return n > 0, err
}

// RecordUpdate logs a detected new-volume event.
func (s *Store) RecordUpdate(bookID int64, oldV, newV int, link string) error {
	_, err := s.db.Exec(
		`INSERT INTO updates(book_id, old_volumes, new_volumes, link, detected_at) VALUES(?,?,?,?,?)`,
		bookID, oldV, newV, link, now())
	return err
}
