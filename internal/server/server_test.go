package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bookwatch/internal/config"
	"bookwatch/internal/notes"
	"bookwatch/internal/provider"
	"bookwatch/internal/scheduler"
	"bookwatch/internal/scraper"
	"bookwatch/internal/service"
	"bookwatch/internal/store"
)

func init() { scraper.AllowPrivateHosts = true } // httptest binds to loopback

func newTestServer(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	vaultDir := t.TempDir()
	cfg := config.Config{Password: "secret", VaultDir: vaultDir, AttachmentsDir: "_attachments"}
	sc := scraper.New("t", 5*time.Second)
	sched := scheduler.New(func(func(i, total int, title string)) (service.CheckSummary, error) {
		return service.CheckSummary{}, nil
	})
	ol := provider.NewOpenLibrary("test", 5*time.Second)
	gb := provider.NewGoogleBooks("", 5*time.Second)
	gr := provider.NewGoodreads("test", 5*time.Second)
	lc := provider.NewLubimyczytac("test", 5*time.Second)
	return New(cfg, st, sc, sched, ol, gb, gr, lc).Handler(), st, vaultDir
}

func do(h http.Handler, method, path, token string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("X-BookWatch-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAuth_writeRequiresToken(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "POST", "/api/check", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}
	if rec := do(h, "POST", "/api/check", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", rec.Code)
	}
	if rec := do(h, "POST", "/api/check", "secret", ""); rec.Code != http.StatusAccepted {
		t.Errorf("correct token: got %d, want 202", rec.Code)
	}
	// The ?token= query fallback was removed — it must no longer authenticate.
	if rec := do(h, "POST", "/api/check?token=secret", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("query-param token should not auth: got %d, want 401", rec.Code)
	}
}

// The LN add-preview endpoint (#52) dry-runs the scrape and must inline the
// cover as a data: URI (the source host isn't in the CSP img-src whitelist),
// while writing no note. It's auth-gated like every other write route.
func TestScrapePreview(t *testing.T) {
	var src *httptest.Server
	src = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cover.webp" {
			w.Header().Set("Content-Type", "image/webp")
			w.Write([]byte("fake-image-bytes"))
			return
		}
		fmt.Fprintf(w, `<html><body>
<h1 class="post-title entry-title">Preview Novel Epub</h1>
<div class="featured-media"><img src="%s/cover.webp"></div>
<div class="synopsis-description"><p>A blurb.</p></div>
<ol><li>VOLUME 1</li><li>VOLUME 2</li></ol>
</body></html>`, src.URL)
	}))
	defer src.Close()

	h, _, _ := newTestServer(t)
	body := fmt.Sprintf(`{"url":%q}`, src.URL)

	if rec := do(h, "POST", "/api/scrape/preview", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}

	rec := do(h, "POST", "/api/scrape/preview", "secret", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Title     string `json:"title"`
		Volumes   int    `json:"volumes"`
		CoverData string `json:"cover_data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "Preview Novel Epub" {
		t.Errorf("title = %q", got.Title)
	}
	if got.Volumes != 2 {
		t.Errorf("volumes = %d, want 2", got.Volumes)
	}
	if !strings.HasPrefix(got.CoverData, "data:image/") {
		t.Errorf("cover_data not an inlined image: %.30q", got.CoverData)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := do(h, "GET", "/", "", "")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("Content-Security-Policy header missing")
	}
}

func TestReadEndpoints_openAndJSON(t *testing.T) {
	h, st, _ := newTestServer(t)
	st.UpsertBook("A", "https://x/1", "", 2, "", "", nil, "ln", "")

	for _, path := range []string{"/api/books", "/api/updates", "/api/runs", "/api/events", "/api/status", "/api/sources", "/api/settings"} {
		rec := do(h, "GET", path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: got %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: content-type %q", path, ct)
		}
	}

	var books []store.Book
	json.Unmarshal(do(h, "GET", "/api/books", "", "").Body.Bytes(), &books)
	if len(books) != 1 || books[0].Title != "A" {
		t.Errorf("books payload: %+v", books)
	}
}

// End-to-end note view/edit (#55): read a book note, edit fields, rename it
// (cover follows, duplicate detection survives), and replace the cover by URL.
func TestNoteViewEdit(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	attach := filepath.Join(vaultDir, "_attachments")
	if err := os.MkdirAll(attach, 0o755); err != nil {
		t.Fatal(err)
	}

	title, link := "My Book", "https://openlibrary.org/works/OL9W"
	cover := notes.CoverName(title, ".jpg")
	notePath := filepath.Join(vaultDir, title+".md")
	content := notes.BuildBookNote(title, "Some Author", link, "OL9W", cover,
		"Backlog", "1990", "Original blurb.", "2026-07-05")
	if err := os.WriteFile(notePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attach, cover), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(title, link, notePath, 0, cover, "Backlog", nil, "book", "Some Author")

	// Read.
	rec := do(h, "GET", fmt.Sprintf("/api/books/%d/note", id), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read: got %d: %s", rec.Code, rec.Body.String())
	}
	n := decodeNote(t, rec)
	if n.Kind != "book" || n.Description != "Original blurb." || n.ReleasedEN != "1990" {
		t.Fatalf("read payload: %+v", n)
	}

	// Edit description + status + tags + released.
	edit := `{"description":"Edited blurb.","status":"Completed","tags":["Favorite"],"released_en":"1991"}`
	if rec := do(h, "PUT", fmt.Sprintf("/api/books/%d/note", id), "secret", edit); rec.Code != http.StatusOK {
		t.Fatalf("edit: got %d: %s", rec.Code, rec.Body.String())
	}
	on := readNote(t, h, id)
	if on.Description != "Edited blurb." || on.Status != "Completed" || on.ReleasedEN != "1991" {
		t.Fatalf("after edit: %+v", on)
	}
	// The defining #Book tag is preserved even though the edit didn't include it.
	hasBook := false
	for _, tg := range on.Tags {
		if tg == "Book" {
			hasBook = true
		}
	}
	if !hasBook {
		t.Errorf("kind tag dropped: %+v", on.Tags)
	}

	// Rename → file moves, cover follows, DB path updates, dup still by link.
	newTitle := "My Renamed Book"
	if rec := do(h, "PUT", fmt.Sprintf("/api/books/%d/note", id), "secret", fmt.Sprintf(`{"title":%q}`, newTitle)); rec.Code != http.StatusOK {
		t.Fatalf("rename: got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(vaultDir, newTitle+".md")); err != nil {
		t.Errorf("renamed note missing: %v", err)
	}
	if _, err := os.Stat(notePath); !os.IsNotExist(err) {
		t.Errorf("old note still present")
	}
	if _, err := os.Stat(filepath.Join(attach, notes.CoverName(newTitle, ".jpg"))); err != nil {
		t.Errorf("renamed cover missing: %v", err)
	}
	if dup, _ := st.BookExists(link); !dup {
		t.Error("duplicate detection broke after rename (link should be unchanged)")
	}

	// Replace cover by URL.
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("PNGDATA"))
	}))
	defer img.Close()
	if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/cover", id), "secret", fmt.Sprintf(`{"url":%q}`, img.URL+"/x.png")); rec.Code != http.StatusOK {
		t.Fatalf("cover: got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(attach, notes.CoverName(newTitle, ".png"))); err != nil {
		t.Errorf("new cover not written: %v", err)
	}
}

type notePayload struct {
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	ReleasedEN  string   `json:"released_en"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
}

func decodeNote(t *testing.T, rec *httptest.ResponseRecorder) notePayload {
	t.Helper()
	var n notePayload
	json.Unmarshal(rec.Body.Bytes(), &n)
	return n
}

func readNote(t *testing.T, h http.Handler, id int64) notePayload {
	t.Helper()
	rec := do(h, "GET", fmt.Sprintf("/api/books/%d/note", id), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read: got %d: %s", rec.Code, rec.Body.String())
	}
	return decodeNote(t, rec)
}

// A multipart cover upload larger than the 1 MiB JSON body cap must still
// succeed — the cover route gets its own larger cap (regression: the generic
// auth cap made real covers fail multipart parsing with "no cover file").
func TestSetCover_multipartOverJSONCap(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	title, link := "Cap Book", "https://openlibrary.org/works/OL7W"
	notePath := filepath.Join(vaultDir, title+".md")
	content := notes.BuildBookNote(title, "A", link, "OL7W", "", "Backlog", "", "", "2026-07-05")
	if err := os.WriteFile(notePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(title, link, notePath, 0, "", "Backlog", nil, "book", "A")

	// ~1.4 MiB payload — over the 1 MiB JSON cap, under the 16 MiB cover cap.
	img := make([]byte, 1_400_000)
	copy(img, []byte("\x89PNG\r\n\x1a\n"))
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("cover", "big, image.png")
	fw.Write(img)
	mw.Close()

	r := httptest.NewRequest("POST", fmt.Sprintf("/api/books/%d/cover", id), &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-BookWatch-Token", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("multipart cover upload: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "_attachments", "cover_CapBook.png")); err != nil {
		t.Errorf("uploaded cover not written: %v", err)
	}
}

func TestDeleteBook_untracksAndLogsEvent(t *testing.T) {
	h, st, _ := newTestServer(t)
	id, _ := st.UpsertBook("A", "https://x/1", "", 2, "", "", nil, "ln", "")

	if rec := do(h, "DELETE", fmt.Sprintf("/api/books/%d", id), "secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if books, _ := st.ListBooks(); len(books) != 0 {
		t.Error("book not deleted")
	}
	evs, _ := st.ListEvents(10)
	if len(evs) != 1 || evs[0].Kind != "untrack" {
		t.Errorf("untrack event missing: %+v", evs)
	}
}

// Hard delete (#58): ?hard=1 removes the note and cover from disk, not just
// the DB row.
func TestDeleteBook_hardDeletesNoteAndCover(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	attach := filepath.Join(vaultDir, "_attachments")
	if err := os.MkdirAll(attach, 0o755); err != nil {
		t.Fatal(err)
	}

	title, link := "Doomed Book", "https://openlibrary.org/works/OL1W"
	cover := notes.CoverName(title, ".jpg")
	notePath := filepath.Join(vaultDir, title+".md")
	content := notes.BuildBookNote(title, "A", link, "OL1W", cover, "Backlog", "", "", "2026-07-05")
	if err := os.WriteFile(notePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	coverPath := filepath.Join(attach, cover)
	if err := os.WriteFile(coverPath, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(title, link, notePath, 0, cover, "Backlog", nil, "book", "A")

	if rec := do(h, "DELETE", fmt.Sprintf("/api/books/%d?hard=1", id), "secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if books, _ := st.ListBooks(); len(books) != 0 {
		t.Error("book not untracked")
	}
	if _, err := os.Stat(notePath); !os.IsNotExist(err) {
		t.Errorf("note not deleted: %v", err)
	}
	if _, err := os.Stat(coverPath); !os.IsNotExist(err) {
		t.Errorf("cover not deleted: %v", err)
	}
	evs, _ := st.ListEvents(10)
	if len(evs) != 1 || evs[0].Kind != "delete" {
		t.Errorf("delete event missing: %+v", evs)
	}
}

func TestWriteBody_sizeCapped(t *testing.T) {
	h, _, _ := newTestServer(t)
	big := `{"url":"` + strings.Repeat("a", 2<<20) + `"}` // > 1 MiB cap
	rec := do(h, "POST", "/api/books", "secret", big)
	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Errorf("oversized body accepted: got %d", rec.Code)
	}
}

func TestAddBook_badBody(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "POST", "/api/books", "secret", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty url should be 400, got %d", rec.Code)
	}
}

func TestBadID_400(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "GET", "/api/cover/notanumber", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad cover id: got %d", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/books/notanumber", "secret", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad book id: got %d", rec.Code)
	}
}

// A cover added after the index was first built must resolve without a restart
// (TTL forced to 0 here so the rebuild happens immediately).
func TestCover_indexInvalidates(t *testing.T) {
	old := coverIdxTTL
	coverIdxTTL = 0
	defer func() { coverIdxTTL = old }()

	h, st, vaultDir := newTestServer(t)
	// Non-standard dir → resolution must go through the vault-wide index, not the
	// fast direct-path check.
	sub := filepath.Join(vaultDir, "elsewhere")
	os.MkdirAll(sub, 0o755)
	id, _ := st.UpsertBook("Late", "https://x/late", "", 1, "late.png", "", nil, "ln", "")

	// Not on disk yet → 404, and the index gets built without it.
	if rec := do(h, "GET", fmt.Sprintf("/api/cover/%d", id), "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("pre-create: got %d, want 404", rec.Code)
	}
	os.WriteFile(filepath.Join(sub, "late.png"), []byte("LATEPNG"), 0o644)
	rec := do(h, "GET", fmt.Sprintf("/api/cover/%d", id), "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "LATEPNG" {
		t.Errorf("post-create without restart: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestCover_serveMissingAndTraversalGuard(t *testing.T) {
	h, st, vaultDir := newTestServer(t)

	// Book with no cover → 404.
	noCover, _ := st.UpsertBook("A", "https://x/1", "", 1, "", "", nil, "ln", "")
	if rec := do(h, "GET", fmt.Sprintf("/api/cover/%d", noCover), "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("no-cover book: got %d", rec.Code)
	}

	// Real cover in the attachments dir → 200 with the bytes.
	attach := filepath.Join(vaultDir, "_attachments")
	os.MkdirAll(attach, 0o755)
	os.WriteFile(filepath.Join(attach, "c.png"), []byte("PNGDATA"), 0o644)
	withCover, _ := st.UpsertBook("B", "https://x/2", "", 1, "c.png", "", nil, "ln", "")
	rec := do(h, "GET", fmt.Sprintf("/api/cover/%d", withCover), "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "PNGDATA" {
		t.Errorf("cover serve: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// A traversal-style cover value is reduced to its basename (filepath.Base),
	// so it cannot escape the vault; the basename isn't present → 404, not a leak.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("TOPSECRET"), 0o644)
	evil, _ := st.UpsertBook("C", "https://x/3", "", 1, "../../../"+secret, "", nil, "ln", "")
	rec = do(h, "GET", fmt.Sprintf("/api/cover/%d", evil), "", "")
	if rec.Body.String() == "TOPSECRET" {
		t.Error("path traversal escaped the vault — cover guard failed")
	}
}
