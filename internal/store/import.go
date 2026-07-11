package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ── Calibre import: resumable session (#75) ──────────────────
//
// The store owns the persistence; the orchestration in internal/importer drives
// it through a small interface these methods satisfy. Session state is kept
// dependency-free (plain columns + JSON strings) so store never imports the
// importer package.

// ImportSession is one import run. status is 'running' (in progress or stopped
// mid-way, resumable), or 'done' (finished — nothing left to process). At most
// one session is ever not 'done'.
type ImportSession struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	LibraryPath   string `json:"library_path"`
	StagingDir    string `json:"staging_dir"`
	StopRequested bool   `json:"stop_requested"`
	Total         int    `json:"total"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
}

// ImportItem is one work unit's persisted state — an LN series (with its
// volumes) or a regular book — keyed by its primary Calibre uuid. uuids is a
// JSON array of every member uuid; candidates and staged_files are JSON too.
type ImportItem struct {
	ID           int64  `json:"id"`
	SessionID    int64  `json:"session_id"`
	Seq          int    `json:"seq"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	UUID         string `json:"uuid"`
	UUIDs        string `json:"uuids"` // JSON array
	State        string `json:"state"` // pending|matched|unmatched|errored
	ResolvedLink string `json:"resolved_link"`
	Candidates   string `json:"candidates"`    // JSON array
	StagedFiles  string `json:"staged_files"`  // JSON array
	DuplicateOf  string `json:"duplicate_of"`  // existing vault note this item duplicates ("" if none)
	Error        string `json:"error"`
}

// ActiveImportSession returns the one session that isn't 'done', if any — the
// session a Resume continues or a Start-over discards.
func (s *Store) ActiveImportSession() (ImportSession, bool, error) {
	return s.scanImportSession(s.db.QueryRow(
		`SELECT id, status, library_path, staging_dir, stop_requested, total, started_at, COALESCE(finished_at,'')
		 FROM import_sessions WHERE status != 'done' ORDER BY id DESC LIMIT 1`))
}

// GetImportSession returns a session by id.
func (s *Store) GetImportSession(id int64) (ImportSession, bool, error) {
	return s.scanImportSession(s.db.QueryRow(
		`SELECT id, status, library_path, staging_dir, stop_requested, total, started_at, COALESCE(finished_at,'')
		 FROM import_sessions WHERE id = ?`, id))
}

// LatestImportSession returns the newest session regardless of status — the one
// the review/finalize flow works against, since a fully-completed run leaves a
// 'done' session that ActiveImportSession (non-'done' only) wouldn't return.
func (s *Store) LatestImportSession() (ImportSession, bool, error) {
	return s.scanImportSession(s.db.QueryRow(
		`SELECT id, status, library_path, staging_dir, stop_requested, total, started_at, COALESCE(finished_at,'')
		 FROM import_sessions ORDER BY id DESC LIMIT 1`))
}

// GetImportItem returns one work-unit item by its row id (the review flow keys
// edits/accepts on it).
func (s *Store) GetImportItem(id int64) (ImportItem, bool, error) {
	var it ImportItem
	err := s.db.QueryRow(`
		SELECT id, session_id, seq, kind, title, uuid, uuids, state, resolved_link, candidates, staged_files, duplicate_of, error
		FROM import_items WHERE id = ?`, id).Scan(
		&it.ID, &it.SessionID, &it.Seq, &it.Kind, &it.Title, &it.UUID, &it.UUIDs,
		&it.State, &it.ResolvedLink, &it.Candidates, &it.StagedFiles, &it.DuplicateOf, &it.Error)
	if err == sql.ErrNoRows {
		return ImportItem{}, false, nil
	}
	return it, err == nil, err
}

// SetImportItemStaged updates one item's staged-files JSON + resolved link +
// state after an in-app review edit (a rename changes the file paths; picking a
// candidate resolves the link and clears the unmatched flag).
func (s *Store) SetImportItemStaged(id int64, stagedFiles, resolvedLink, state, duplicateOf string) error {
	_, err := s.db.Exec(
		`UPDATE import_items SET staged_files=?, resolved_link=?, state=?, duplicate_of=? WHERE id=?`,
		stagedFiles, resolvedLink, state, duplicateOf, id)
	return err
}

func (s *Store) scanImportSession(row *sql.Row) (ImportSession, bool, error) {
	var se ImportSession
	var stop int
	err := row.Scan(&se.ID, &se.Status, &se.LibraryPath, &se.StagingDir, &stop, &se.Total, &se.StartedAt, &se.FinishedAt)
	if err == sql.ErrNoRows {
		return ImportSession{}, false, nil
	}
	if err != nil {
		return ImportSession{}, false, err
	}
	se.StopRequested = stop != 0
	return se, true, nil
}

// CreateImportSession opens a fresh running session. It refuses when one is
// already active (one active session at a time).
func (s *Store) CreateImportSession(libraryPath, stagingDir string) (int64, error) {
	if _, ok, err := s.ActiveImportSession(); err != nil {
		return 0, err
	} else if ok {
		return 0, fmt.Errorf("an import session is already active")
	}
	res, err := s.db.Exec(
		`INSERT INTO import_sessions(status, library_path, staging_dir, started_at) VALUES('running',?,?,?)`,
		libraryPath, stagingDir, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetImportTotal records how many work units the session will process, for the
// "N/total" progress readout.
func (s *Store) SetImportTotal(id int64, total int) error {
	_, err := s.db.Exec(`UPDATE import_sessions SET total=? WHERE id=?`, total, id)
	return err
}

// RequestImportStop flags the running session to halt after the current item.
func (s *Store) RequestImportStop(id int64) error {
	_, err := s.db.Exec(`UPDATE import_sessions SET stop_requested=1 WHERE id=?`, id)
	return err
}

// ClearImportStop clears the stop flag (on resume, so the run proceeds).
func (s *Store) ClearImportStop(id int64) error {
	_, err := s.db.Exec(`UPDATE import_sessions SET stop_requested=0 WHERE id=?`, id)
	return err
}

// ImportStopRequested reports whether a stop was requested for the session.
func (s *Store) ImportStopRequested(id int64) (bool, error) {
	var stop int
	err := s.db.QueryRow(`SELECT stop_requested FROM import_sessions WHERE id=?`, id).Scan(&stop)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return stop != 0, err
}

// FinishImportSession stamps a terminal status ('done' when everything is
// processed, 'running' left as-is when merely stopped mid-way).
func (s *Store) FinishImportSession(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE import_sessions SET status=?, finished_at=? WHERE id=?`, status, now(), id)
	return err
}

// DeleteImportSession removes a session and its items (cascade). Start-over
// calls this after wiping the app-written staged files + processed uuids.
func (s *Store) DeleteImportSession(id int64) error {
	_, err := s.db.Exec(`DELETE FROM import_sessions WHERE id=?`, id)
	return err
}

// UpsertImportItem inserts or replaces one work unit's state by (session, uuid).
func (s *Store) UpsertImportItem(it ImportItem) error {
	_, err := s.db.Exec(`
		INSERT INTO import_items(session_id, seq, kind, title, uuid, uuids, state, resolved_link, candidates, staged_files, duplicate_of, error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id, uuid) DO UPDATE SET
			seq=excluded.seq, kind=excluded.kind, title=excluded.title, uuids=excluded.uuids,
			state=excluded.state, resolved_link=excluded.resolved_link, candidates=excluded.candidates,
			staged_files=excluded.staged_files, duplicate_of=excluded.duplicate_of, error=excluded.error`,
		it.SessionID, it.Seq, it.Kind, it.Title, it.UUID, it.UUIDs, it.State,
		it.ResolvedLink, it.Candidates, it.StagedFiles, it.DuplicateOf, it.Error)
	return err
}

// ListImportItems returns a session's items in processing order.
func (s *Store) ListImportItems(sessionID int64) ([]ImportItem, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, seq, kind, title, uuid, uuids, state, resolved_link, candidates, staged_files, duplicate_of, error
		FROM import_items WHERE session_id=? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportItem
	for rows.Next() {
		var it ImportItem
		if err := rows.Scan(&it.ID, &it.SessionID, &it.Seq, &it.Kind, &it.Title, &it.UUID, &it.UUIDs,
			&it.State, &it.ResolvedLink, &it.Candidates, &it.StagedFiles, &it.DuplicateOf, &it.Error); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ResetImportItems flips items in any of the given states back to 'pending' so
// they're reprocessed — the "Retry errored/unmatched" action.
func (s *Store) ResetImportItems(sessionID int64, states []string) error {
	if len(states) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := []any{sessionID}
	for _, st := range states {
		args = append(args, st)
	}
	_, err := s.db.Exec(
		`UPDATE import_items SET state='pending', error='' WHERE session_id=? AND state IN (`+ph+`)`, args...)
	return err
}

// MarkProcessedUUIDs records Calibre uuids as handled (idempotency). Duplicate
// uuids are ignored, so re-processing a session item can't error.
func (s *Store) MarkProcessedUUIDs(uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	for _, u := range uuids {
		if u == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO import_processed(uuid, processed_at) VALUES(?,?) ON CONFLICT(uuid) DO NOTHING`, u, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ForgetProcessedUUIDs drops uuids from the processed set — Start-over calls
// this for its session's items so a re-run sees those books as new again.
func (s *Store) ForgetProcessedUUIDs(uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range uuids {
		if u == "" {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM import_processed WHERE uuid=?`, u); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResetImport wipes all import state — every session (+ its items) and the whole
// processed-uuid idempotency set — for a clean slate. Used when the staging
// folder has been abandoned (deleted / emptied of notes), so a fresh import
// doesn't keep reporting the old books as already-done. Files on disk are the
// caller's concern; this is DB-only.
func (s *Store) ResetImport() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM import_items`,
		`DELETE FROM import_sessions`,
		`DELETE FROM import_processed`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProcessedUUIDs returns the set of every Calibre uuid handled by a prior
// completed session, so enumeration can skip them and only pick up new books.
func (s *Store) ProcessedUUIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT uuid FROM import_processed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}
