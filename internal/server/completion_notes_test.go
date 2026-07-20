package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bookwatch/internal/notes"
)

// notesUnsaved decodes the `notes_unsaved` warning flag from a completion/edit
// response body.
func notesUnsaved(t *testing.T, body string) bool {
	t.Helper()
	var out struct {
		NotesUnsaved bool `json:"notes_unsaved"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return out.NotesUnsaved
}

// A #Book completion writes the personal notes into the book's own note, and the
// edit-entry path can then load and rewrite that section.
func TestCompletionNotes_bookNote(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	if err := os.WriteFile(logPath, []byte("---\nmodified: 2026-07-06\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.SetSetting("reading_log_path", logPath)

	title, link := "My Book", "https://openlibrary.org/works/OL9W"
	notePath := filepath.Join(vaultDir, title+".md")
	content := notes.BuildBookNote(title, "A", link, "OL9W", "", "Backlog", "", "", "2026-07-05")
	if err := os.WriteFile(notePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(title, link, notePath, 0, "", "Backlog", nil, "book", "A")

	body := `{"start":"2026-07-01","end":"2026-07-06","notes":"A quiet, tidy read."}`
	rec := do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", body)
	if rec.Code != 200 {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	if notesUnsaved(t, rec.Body.String()) {
		t.Error("book note exists — notes_unsaved should be false")
	}
	if got := string(mustRead(t, notePath)); !strings.Contains(got, "## Notes\n\nA quiet, tidy read.") {
		t.Errorf("notes not written to book note:\n%s", got)
	}

	// The edit dialog loads the current text back.
	get := do(h, "GET", "/api/reading/notes?title="+url.QueryEscape(title)+"&volume=", "", "")
	if get.Code != 200 || !strings.Contains(get.Body.String(), "A quiet, tidy read.") {
		t.Fatalf("GET notes: %d %s", get.Code, get.Body.String())
	}

	// Editing the log entry rewrites the section (row 0 in file order).
	edit := `{"index":0,"title":"My Book","start":"2026-07-01","end":"2026-07-06","notes":"Revised: a gem."}`
	if rec := do(h, "PUT", "/api/reading/completed", "secret", edit); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	got := string(mustRead(t, notePath))
	if strings.Count(got, "## Notes") != 1 || !strings.Contains(got, "Revised: a gem.") || strings.Contains(got, "A quiet, tidy read.") {
		t.Errorf("edit did not replace the notes section:\n%s", got)
	}
}

// An LN volume completion writes notes into the per-volume note when it exists,
// and warns (notes_unsaved) when the volume was never backfilled.
func TestCompletionNotes_lnVolumeNote(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	if err := os.WriteFile(logPath, []byte("| 202506 | [[Other]] | 1 |  |  |\n| --- | --- | ---: | --- | --- |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.SetSetting("reading_log_path", logPath)

	series, link := "Hell Mode", "https://jnovels.com/hm"
	notePath := filepath.Join(vaultDir, series+".md")
	if err := os.WriteFile(notePath, []byte("---\ntags:\n  - \"#LightNovel\"\nStatus:\n  - Backlog\n---\n### Hell Mode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(series, link, notePath, 10, "", "Backlog", nil, "ln", "")

	// Volume 3 has a backfilled note; volume 4 does not.
	volPath := notes.VolumePath(notePath, series, 3)
	if err := os.MkdirAll(filepath.Dir(volPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(volPath, []byte(notes.BuildLNVolumeNote(series, 3, 10, "", "", "", "", "", "2026-07-17", false)), 0o644); err != nil {
		t.Fatal(err)
	}

	// Volume 3: notes land in the volume note, above the nav footer, no warning.
	rec := do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", `{"volume":"3","unknown":true,"notes":"Turning point."}`)
	if rec.Code != 200 {
		t.Fatalf("complete v3: %d %s", rec.Code, rec.Body.String())
	}
	if notesUnsaved(t, rec.Body.String()) {
		t.Error("volume 3 note exists — notes_unsaved should be false")
	}
	got := string(mustRead(t, volPath))
	if !strings.Contains(got, "## Notes\n\nTurning point.") {
		t.Errorf("notes not written to volume note:\n%s", got)
	}
	if strings.Index(got, "## Notes") > strings.Index(got, "Next:") {
		t.Errorf("notes landed below the nav footer:\n%s", got)
	}
	// The series note itself must not gain a Notes section.
	if strings.Contains(string(mustRead(t, notePath)), "## Notes") {
		t.Errorf("series note wrongly got the notes:\n%s", mustRead(t, notePath))
	}

	// Volume 4: no note on disk → completion still succeeds but warns.
	rec = do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", `{"volume":"4","unknown":true,"notes":"Should warn."}`)
	if rec.Code != 200 {
		t.Fatalf("complete v4: %d %s", rec.Code, rec.Body.String())
	}
	if !notesUnsaved(t, rec.Body.String()) {
		t.Error("volume 4 has no note — notes_unsaved should be true")
	}

	// GET notes resolves the volume note by title + volume.
	get := do(h, "GET", "/api/reading/notes?title="+url.QueryEscape(series)+"&volume=3", "", "")
	if get.Code != 200 || !strings.Contains(get.Body.String(), "Turning point.") {
		t.Fatalf("GET volume notes: %d %s", get.Code, get.Body.String())
	}
}
