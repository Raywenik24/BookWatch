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
	// Pragmas live in the DSN so they apply to EVERY connection the pool opens,
	// not just the first. foreign_keys and busy_timeout are per-connection
	// settings: running them once via db.Exec would leave every other pooled
	// connection with foreign_keys=OFF (breaking ON DELETE CASCADE → orphan
	// updates/rules rows) and busy_timeout=0 (instant "database is locked" when
	// the scheduler goroutine and an HTTP write collide). journal_mode=WAL is
	// persisted in the file header, but is set here too for a fresh DB. modernc
	// runs each _pragma on connect.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
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
	// v2: pending-updates model. An update is "pending" until applied to the
	// vault; applying stamps applied=1 + applied_at. Re-detection reuses the
	// pending row (see UpsertPendingUpdate), so at most one pending per book.
	`ALTER TABLE updates ADD COLUMN applied    INTEGER NOT NULL DEFAULT 0;
	 ALTER TABLE updates ADD COLUMN applied_at TEXT;`,
	// v3: activity/event log — discrete actions (book added/untracked, updates
	// applied, auto-prune) so the Logs tab shows more than just check runs.
	`CREATE TABLE events (
		id      INTEGER PRIMARY KEY,
		at      TEXT NOT NULL,
		kind    TEXT NOT NULL,
		message TEXT NOT NULL
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

// UpsertBook inserts or updates a book by link, returning its id. An empty
// cover never clears an existing one — a re-check before the next note parse
// keeps whatever was already stored.
func (s *Store) UpsertBook(title, link, path string, volumes int, cover string) (int64, error) {
	ts := now()
	_, err := s.db.Exec(`
		INSERT INTO books(title, link, path, volumes, cover, created_at, updated_at, last_checked_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(link) DO UPDATE SET
			title=excluded.title, path=excluded.path,
			volumes=excluded.volumes,
			cover=CASE WHEN excluded.cover='' THEN books.cover ELSE excluded.cover END,
			updated_at=excluded.updated_at,
			last_checked_at=excluded.last_checked_at`,
		title, link, path, volumes, cover, ts, ts, ts)
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
	Cover         string `json:"cover"`
	UpdatedAt     string `json:"updated_at"`
	LastCheckedAt string `json:"last_checked_at"`
}

func (s *Store) ListBooks() ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, title, link, path, volumes, cover, updated_at,
		COALESCE(last_checked_at,'') FROM books ORDER BY title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Title, &b.Link, &b.Path, &b.Volumes, &b.Cover, &b.UpdatedAt, &b.LastCheckedAt); err != nil {
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
	Applied    bool   `json:"applied"`
	AppliedAt  string `json:"applied_at"`
}

func (s *Store) ListUpdates(limit int) ([]Update, error) {
	rows, err := s.db.Query(`SELECT u.id, b.title, u.old_volumes, u.new_volumes, u.link,
		u.detected_at, u.applied, COALESCE(u.applied_at,'')
		FROM updates u JOIN books b ON b.id = u.book_id
		ORDER BY u.detected_at DESC, u.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		var u Update
		var applied int
		if err := rows.Scan(&u.ID, &u.Title, &u.OldVolumes, &u.NewVolumes, &u.Link,
			&u.DetectedAt, &applied, &u.AppliedAt); err != nil {
			return nil, err
		}
		u.Applied = applied != 0
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

// ── events (activity log) ─────────────────────────────────────

type Event struct {
	ID      int64  `json:"id"`
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// LogEvent records one discrete action (book added/untracked, updates applied,
// auto-prune). Best-effort: callers ignore the error so logging never breaks
// the action it describes.
func (s *Store) LogEvent(kind, message string) error {
	_, err := s.db.Exec(`INSERT INTO events(at, kind, message) VALUES(?,?,?)`, now(), kind, message)
	return err
}

func (s *Store) ListEvents(limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, at, kind, message FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
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

// BookCover returns a book's cover attachment filename (empty if none / no row).
func (s *Store) BookCover(id int64) (string, error) {
	var cover string
	err := s.db.QueryRow(`SELECT cover FROM books WHERE id=?`, id).Scan(&cover)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return cover, err
}

// BookTitle returns a book's title (empty if no row). Used to name an untrack
// event before the row is deleted.
func (s *Store) BookTitle(id int64) (string, error) {
	var title string
	err := s.db.QueryRow(`SELECT title FROM books WHERE id=?`, id).Scan(&title)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return title, err
}

// DeleteBook removes a book's DB row (and its updates, via cascade). The vault
// note and cover are never touched — vault stays the source of truth.
func (s *Store) DeleteBook(id int64) error {
	_, err := s.db.Exec(`DELETE FROM books WHERE id=?`, id)
	return err
}

// UpsertPendingUpdate records a detected new-volume event as PENDING. There is
// at most one pending row per book: a re-detection refreshes the existing
// pending row instead of stacking duplicates. Applied rows are history and are
// left alone, so a fresh detection after an apply opens a new pending row.
func (s *Store) UpsertPendingUpdate(bookID int64, oldV, newV int, link string) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE updates SET old_volumes=?, new_volumes=?, link=?, detected_at=?
		 WHERE book_id=? AND applied=0`,
		oldV, newV, link, now(), bookID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		var id int64
		err = s.db.QueryRow(`SELECT id FROM updates WHERE book_id=? AND applied=0`, bookID).Scan(&id)
		return id, err
	}
	res, err = s.db.Exec(
		`INSERT INTO updates(book_id, old_volumes, new_volumes, link, detected_at) VALUES(?,?,?,?,?)`,
		bookID, oldV, newV, link, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PendingUpdate is a not-yet-applied bump joined to its book's note path.
type PendingUpdate struct {
	ID         int64
	BookID     int64
	Title      string
	Path       string
	Link       string
	OldVolumes int
	NewVolumes int
}

// ListPending returns every pending (un-applied) update with its book's path.
func (s *Store) ListPending() ([]PendingUpdate, error) {
	rows, err := s.db.Query(`SELECT u.id, u.book_id, b.title, b.path, u.link, u.old_volumes, u.new_volumes
		FROM updates u JOIN books b ON b.id = u.book_id
		WHERE u.applied=0 ORDER BY b.title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingUpdate
	for rows.Next() {
		var p PendingUpdate
		if err := rows.Scan(&p.ID, &p.BookID, &p.Title, &p.Path, &p.Link, &p.OldVolumes, &p.NewVolumes); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPending returns how many updates are waiting to be applied.
func (s *Store) CountPending() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM updates WHERE applied=0`).Scan(&n)
	return n, err
}

// MarkApplied stamps an update applied and bumps its book to newVolumes.
func (s *Store) MarkApplied(updateID, bookID int64, newVolumes int) error {
	ts := now()
	if _, err := s.db.Exec(
		`UPDATE updates SET applied=1, applied_at=? WHERE id=?`, ts, updateID); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`UPDATE books SET volumes=?, updated_at=? WHERE id=?`, newVolumes, ts, bookID)
	return err
}
