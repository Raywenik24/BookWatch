package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"bookwatch/internal/importer"
	"bookwatch/internal/store"

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

func TestImportReconcileDiscardsOnDeletedStaging(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	staging := filepath.Join(vaultDir, "_CalibreImport")
	res, err := importer.NewWriter(staging, "2026-07-10").
		StageBook(importer.PlanBook{Title: "Kept", Link: "https://openlibrary.org/works/OLxW"})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := st.CreateImportSession("lib", staging)
	if err != nil {
		t.Fatal(err)
	}
	st.FinishImportSession(sid, "done")
	st.UpsertImportItem(store.ImportItem{SessionID: sid, Title: "Kept", UUID: "u-a", UUIDs: `["u-a"]`, State: "matched", StagedFiles: stagedJSON(res)})
	st.MarkProcessedUUIDs([]string{"u-a"}) // the "already done" set

	// While the staging folder exists, status reports the finished session.
	rec := do(h, "GET", "/api/import/calibre/status", "", "")
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["state"] != "review" {
		t.Fatalf("with staging present, state = %v, want review", got["state"])
	}

	// Delete the staging folder → the next status read reconciles it away.
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	rec = do(h, "GET", "/api/import/calibre/status", "", "")
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["state"] != "idle" {
		t.Errorf("after deleting staging, state = %v, want idle", got["state"])
	}
	if proc, _ := st.ProcessedUUIDs(); len(proc) != 0 {
		t.Errorf("processed uuids not forgotten: %v", proc)
	}
	if _, ok, _ := st.LatestImportSession(); ok {
		t.Error("session should have been discarded")
	}
}

// The staging folder can survive (e.g. its report note) while the staged book
// notes are deleted — that emptied-of-notes state also abandons the session.
func TestImportReconcileDiscardsWhenNotesDeleted(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	staging := filepath.Join(vaultDir, "_CalibreImport")
	res, err := importer.NewWriter(staging, "2026-07-10").
		StageBook(importer.PlanBook{Title: "Kept", Link: "https://openlibrary.org/works/OLxW"})
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := st.CreateImportSession("lib", staging)
	st.FinishImportSession(sid, "done")
	st.UpsertImportItem(store.ImportItem{SessionID: sid, Title: "Kept", UUID: "u-a", UUIDs: `["u-a"]`, State: "matched", StagedFiles: stagedJSON(res)})
	st.MarkProcessedUUIDs([]string{"u-a"})

	// Delete just the staged note, leaving the folder (a leftover report file).
	os.Remove(res.Note)
	if err := os.WriteFile(filepath.Join(staging, "_CalibreImport-report.md"), []byte("# report\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(h, "GET", "/api/import/calibre/status", "", "")
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["state"] != "idle" {
		t.Errorf("with all notes gone, state = %v, want idle", got["state"])
	}
	if proc, _ := st.ProcessedUUIDs(); len(proc) != 0 {
		t.Errorf("processed uuids not forgotten: %v", proc)
	}
}

// stagedJSON is the staged_files record for a StageResult (note + volumes +
// covers), as the orchestrator writes it.
func stagedJSON(r importer.StageResult) string {
	files := append([]string{r.Note}, r.VolumeNotes...)
	files = append(files, r.Covers...)
	b, _ := json.Marshal(files)
	return string(b)
}

func TestImportReviewFlow(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	for k, v := range map[string]string{
		"new_note_dir": "01_LN", "book_new_note_dir": "02_Books",
		"attachments_dir": "01_LN/_attachments", "book_attachments_dir": "02_Books/_attachments",
	} {
		st.SetSetting(k, v)
	}

	staging := filepath.Join(vaultDir, "_CalibreImport")
	w := importer.NewWriter(staging, "2026-07-10")
	un, err := w.StageBook(importer.PlanBook{Title: "Sixty One", Author: "Lee Child", Unmatched: true,
		Candidates: []importer.Candidate{{Title: "61 hours", URL: "https://lubimyczytac.pl/ksiazka/1/61-godzin"}}})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := w.StageBook(importer.PlanBook{Title: "Clean One", Link: "https://openlibrary.org/works/OLxW"})
	if err != nil {
		t.Fatal(err)
	}

	sid, err := st.CreateImportSession("lib", staging)
	if err != nil {
		t.Fatal(err)
	}
	st.FinishImportSession(sid, "done") // a completed run
	st.UpsertImportItem(store.ImportItem{SessionID: sid, Seq: 0, Kind: "polish", Title: "Sixty One", UUID: "u-un",
		State: "unmatched", Candidates: `[{"title":"61 hours","url":"https://lubimyczytac.pl/ksiazka/1/61-godzin"}]`, StagedFiles: stagedJSON(un)})
	st.UpsertImportItem(store.ImportItem{SessionID: sid, Seq: 1, Kind: "english", Title: "Clean One", UUID: "u-cl",
		State: "matched", ResolvedLink: "https://openlibrary.org/works/OLxW", StagedFiles: stagedJSON(cl)})

	// A completed run reports the 'review' state (not idle) so the reviewer shows.
	rec := do(h, "GET", "/api/import/calibre/status", "", "")
	var status map[string]any
	json.Unmarshal(rec.Body.Bytes(), &status)
	if status["state"] != "review" {
		t.Fatalf("status state = %v, want review", status["state"])
	}

	// The review queue lists both, with the right flags.
	rec = do(h, "GET", "/api/import/calibre/review", "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("review list = %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items  []map[string]any `json:"items"`
		Counts map[string]int   `json:"counts"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Counts["unmatched"] != 1 || list.Counts["clean"] != 1 {
		t.Fatalf("review counts = %+v", list.Counts)
	}

	items, _ := st.ListImportItems(sid)
	var unID, clID int64
	for _, it := range items {
		if it.Title == "Sixty One" {
			unID = it.ID
		} else {
			clID = it.ID
		}
	}

	// Item detail carries the candidates + unmatched flag.
	rec = do(h, "GET", "/api/import/calibre/review/item?id="+strconv.FormatInt(unID, 10), "secret", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "61 hours") {
		t.Fatalf("review item = %d: %s", rec.Code, rec.Body.String())
	}

	// Manually pasting a real Link (not via a candidate) must resolve the note:
	// drop #import/unmatched and fill Work ID from the URL.
	rec = do(h, "PUT", "/api/import/calibre/review/item", "secret",
		`{"id":`+strconv.FormatInt(unID, 10)+`,"link":"https://lubimyczytac.pl/ksiazka/4945864/61-godzin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual link edit = %d: %s", rec.Code, rec.Body.String())
	}
	unItem, _, _ := st.GetImportItem(unID)
	nb, _ := os.ReadFile(primaryNote(parseStagedFiles(unItem.StagedFiles)))
	if strings.Contains(string(nb), "#import/unmatched") {
		t.Errorf("manual link should drop #import/unmatched tag:\n%s", nb)
	}
	if !strings.Contains(string(nb), "Work ID: 4945864") {
		t.Errorf("manual link should fill Work ID from the lubimyczytac URL:\n%s", nb)
	}

	// Edit the clean note's status + description, then accept it → moved to 02_Books.
	rec = do(h, "PUT", "/api/import/calibre/review/item", "secret",
		`{"id":`+strconv.FormatInt(clID, 10)+`,"status":"Completed","description":"hand-written blurb"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit = %d: %s", rec.Code, rec.Body.String())
	}
	body, _ := os.ReadFile(cl.Note)
	if !strings.Contains(string(body), "Completed") || !strings.Contains(string(body), "hand-written blurb") {
		t.Errorf("edit not applied to staged note:\n%s", body)
	}

	rec = do(h, "POST", "/api/import/calibre/review/accept", "secret", `{"id":`+strconv.FormatInt(clID, 10)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "02_Books", "Clean One.md")); err != nil {
		t.Errorf("accepted note not moved to 02_Books: %v", err)
	}
	if _, err := os.Stat(cl.Note); !os.IsNotExist(err) {
		t.Errorf("accepted note still in staging")
	}

	// Reject the unmatched note → its staged files are deleted, nothing moved.
	rec = do(h, "POST", "/api/import/calibre/review/reject", "secret", `{"id":`+strconv.FormatInt(unID, 10)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(un.Note); !os.IsNotExist(err) {
		t.Errorf("rejected note still in staging")
	}

	// Auth is required on a review write.
	if rec := do(h, "POST", "/api/import/calibre/review/accept", "", `{"id":1}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("accept without token: got %d, want 401", rec.Code)
	}
}
