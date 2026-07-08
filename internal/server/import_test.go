package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"bookwatch/internal/importer"

	_ "modernc.org/sqlite"
)

// buildCalibreFixture writes a tiny metadata.db with one LN series (two owned
// volumes) and one bare book, enough to exercise the import grouping offline.
func buildCalibreFixture(t *testing.T, root string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT, sort TEXT, author_sort TEXT,
			uuid TEXT, path TEXT, has_cover INTEGER, series_index REAL, pubdate TEXT, timestamp TEXT);
		 CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT, sort TEXT);
		 CREATE TABLE books_authors_link (id INTEGER PRIMARY KEY, book INTEGER, author INTEGER);
		 CREATE TABLE series (id INTEGER PRIMARY KEY, name TEXT, sort TEXT);
		 CREATE TABLE books_series_link (id INTEGER PRIMARY KEY, book INTEGER, series INTEGER);
		 CREATE TABLE languages (id INTEGER PRIMARY KEY, lang_code TEXT);
		 CREATE TABLE books_languages_link (id INTEGER PRIMARY KEY, book INTEGER, lang_code INTEGER, item_order INTEGER);
		 CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT);
		 CREATE TABLE books_tags_link (id INTEGER PRIMARY KEY, book INTEGER, tag INTEGER);
		 CREATE TABLE identifiers (id INTEGER PRIMARY KEY, book INTEGER, type TEXT, val TEXT);
		 CREATE TABLE comments (id INTEGER PRIMARY KEY, book INTEGER, text TEXT);`,
		`INSERT INTO books VALUES
			(1,'Chronicle Vol 1','','A','u-1','A/v1',0,1.0,'2021-01-01','2021-01-01'),
			(2,'Chronicle Vol 2','','A','u-2','A/v2',0,2.0,'2021-06-01','2021-06-01'),
			(3,'Loner','','B','u-3','B/loner',0,0,'2020-01-01','2020-01-01')`,
		`INSERT INTO authors VALUES (1,'Aki','Aki'),(2,'Ben','Ben')`,
		`INSERT INTO books_authors_link VALUES (1,1,1),(2,2,1),(3,3,2)`,
		`INSERT INTO series VALUES (1,'The Chronicle','')`,
		`INSERT INTO books_series_link VALUES (1,1,1),(2,2,1)`,
		`INSERT INTO languages VALUES (1,'eng')`,
		`INSERT INTO books_languages_link VALUES (1,1,1,0),(2,2,1,0),(3,3,1,0)`,
		`INSERT INTO tags VALUES (1,'Light Novel')`,
		`INSERT INTO books_tags_link VALUES (1,1,1),(2,2,1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("fixture exec: %v", err)
		}
	}
}

func TestImportStatusIdle(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := do(h, "GET", "/api/import/calibre/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["state"] != "idle" {
		t.Errorf("state = %v, want idle", got["state"])
	}
}

func TestImportPreviewRequiresLibrary(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "POST", "/api/import/calibre/preview", "secret", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("preview without library: got %d, want 400", rec.Code)
	}
	// Auth is required.
	if rec := do(h, "POST", "/api/import/calibre/preview", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("preview without token: got %d, want 401", rec.Code)
	}
}

func TestImportPreviewCounts(t *testing.T) {
	h, st, _ := newTestServer(t)
	lib := t.TempDir()
	buildCalibreFixture(t, lib)
	if err := st.SetSetting("calibre_library_path", lib); err != nil {
		t.Fatal(err)
	}
	rec := do(h, "POST", "/api/import/calibre/preview", "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d: %s", rec.Code, rec.Body.String())
	}
	var p struct {
		LNSeries     int `json:"ln_series"`
		LNVolumes    int `json:"ln_volumes"`
		RegularBooks int `json:"regular_books"`
	}
	json.Unmarshal(rec.Body.Bytes(), &p)
	if p.LNSeries != 1 || p.LNVolumes != 2 || p.RegularBooks != 1 {
		t.Errorf("preview counts = %+v, want 1 series / 2 volumes / 1 book", p)
	}
}

func TestImportStartRequiresLibrary(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "POST", "/api/import/calibre", "secret", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("start without library: got %d, want 400", rec.Code)
	}
}

func TestImportStopNoSession(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "POST", "/api/import/calibre/stop", "secret", ""); rec.Code != http.StatusOK {
		t.Errorf("stop with no session: got %d, want 200", rec.Code)
	}
}

func TestImportFinalizeMovesStagedNotes(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	for k, v := range map[string]string{
		"new_note_dir":      "01_LightNovel_db",
		"book_new_note_dir": "02_Books_db",
		"attachments_dir":   "01_LightNovel_db/_attachments",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	// Stage a matched book into the default staging dir (vault/_CalibreImport).
	staging := filepath.Join(vaultDir, "_CalibreImport")
	w := importer.NewWriter(staging, "2026-07-08")
	if _, err := w.StageBook(importer.PlanBook{Title: "Migrated", Link: "https://openlibrary.org/works/OLxW"}); err != nil {
		t.Fatal(err)
	}

	rec := do(h, "POST", "/api/import/calibre/finalize", "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("finalize = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Notes       int    `json:"notes"`
		ExcludeHint string `json:"exclude_hint"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Notes != 1 {
		t.Errorf("moved %d notes, want 1", got.Notes)
	}
	if got.ExcludeHint != "01_LightNovel_db/_volumes" {
		t.Errorf("exclude hint = %q", got.ExcludeHint)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "02_Books_db", "Migrated.md")); err != nil {
		t.Errorf("book note not moved to 02_Books_db: %v", err)
	}
	// Auth is required.
	if rec := do(h, "POST", "/api/import/calibre/finalize", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("finalize without token: got %d, want 401", rec.Code)
	}
}
