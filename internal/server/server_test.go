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
	noop := func(func(i, total int, title string)) (service.CheckSummary, error) {
		return service.CheckSummary{}, nil
	}
	sched := scheduler.New(noop, noop, noop)
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

// Client routes (#62) are served the SPA shell so a refresh/bookmark on
// /books, /updates?kind=ln, etc. loads the app; only unmatched /api/ paths 404.
func TestClientRoutesServeIndex(t *testing.T) {
	h, _, _ := newTestServer(t)
	for _, p := range []string{"/", "/books", "/updates", "/randomizer", "/settings", "/books?kind=ln"} {
		rec := do(h, "GET", p, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d, want 200", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s: content-type %q, want text/html", p, ct)
		}
	}
	// Unmatched API paths are still genuine 404s, not the SPA shell.
	if rec := do(h, "GET", "/api/nope", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/nope: got %d, want 404", rec.Code)
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

// Reading log (#63): marking a #Book completed appends a row to the configured
// log AND flips its note Status to Completed; the completed feed reports the
// re-read count.
func TestMarkCompleted_bookFlipsStatusAndLogs(t *testing.T) {
	h, st, vaultDir := newTestServer(t)

	logPath := filepath.Join(vaultDir, "_Read.md")
	if err := os.WriteFile(logPath, []byte("---\nmodified: 2026-07-06\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("reading_log_path", logPath); err != nil {
		t.Fatal(err)
	}

	title, link := "My Book", "https://openlibrary.org/works/OL9W"
	notePath := filepath.Join(vaultDir, title+".md")
	content := notes.BuildBookNote(title, "A", link, "OL9W", "", "Backlog", "", "", "2026-07-05")
	if err := os.WriteFile(notePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(title, link, notePath, 0, "", "Backlog", nil, "book", "A")

	body := `{"start":"2026-07-01","end":"2026-07-06"}`
	rec := do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: got %d: %s", rec.Code, rec.Body.String())
	}

	// A compact row landed in the log with the note basename as the [[link]].
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "[[My Book]]") || !strings.Contains(string(raw), "202607") {
		t.Fatalf("row not appended:\n%s", raw)
	}
	// #Book completion flipped Status to Completed on disk + in the DB.
	if !strings.Contains(string(mustRead(t, notePath)), "Completed") {
		t.Errorf("note Status not set to Completed:\n%s", mustRead(t, notePath))
	}
	books, _ := st.ListBooks()
	if len(books) != 1 || books[0].Status != "Completed" {
		t.Errorf("DB status not resynced: %+v", books)
	}
	evs, _ := st.ListEvents(10)
	if len(evs) == 0 || evs[0].Kind != "read" {
		t.Errorf("read event missing: %+v", evs)
	}
}

// An LN volume completion logs the read but must NOT touch the note's Status.
func TestMarkCompleted_lnLeavesStatusUntouched(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	if err := os.WriteFile(logPath, []byte("| 202506 | [[Other]] | 1 |  |  |\n| --- | --- | ---: | --- | --- |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.SetSetting("reading_log_path", logPath)

	title, link := "Hell Mode", "https://jnovels.com/hm"
	notePath := filepath.Join(vaultDir, title+".md")
	if err := os.WriteFile(notePath, []byte("---\ntags:\n  - \"#LightNovel\"\nStatus:\n  - Backlog\n---\n### Hell Mode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(title, link, notePath, 10, "", "Backlog", nil, "ln", "")

	// Complete the same volume twice → re-read count of 2.
	for i := 0; i < 2; i++ {
		body := `{"volume":"10","unknown":true}`
		if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", body); rec.Code != http.StatusOK {
			t.Fatalf("complete %d: got %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	// Status line untouched (still Backlog, never Completed).
	if strings.Contains(string(mustRead(t, notePath)), "Completed") {
		t.Errorf("LN completion wrongly touched Status:\n%s", mustRead(t, notePath))
	}
	// Unknown-date rows have blank date + YYYYMM cells.
	if !strings.Contains(string(mustRead(t, logPath)), "|  | [[Hell Mode]] | 10 |  |  |") {
		t.Errorf("unknown-date LN row wrong:\n%s", mustRead(t, logPath))
	}

	// Completed feed surfaces the re-read (count ≥ 2).
	rec := do(h, "GET", "/api/reading/completed", "", "")
	var feed struct {
		Configured bool `json:"configured"`
		Reread     []struct {
			Title  string `json:"title"`
			Volume string `json:"volume"`
			Count  int    `json:"count"`
		} `json:"reread"`
	}
	json.Unmarshal(rec.Body.Bytes(), &feed)
	if !feed.Configured {
		t.Fatal("feed not configured")
	}
	var found bool
	for _, rr := range feed.Reread {
		if rr.Title == "Hell Mode" && rr.Volume == "10" && rr.Count == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("re-read of Hell Mode ×10 not surfaced: %+v", feed.Reread)
	}
}

// The complete endpoint refuses cleanly when no reading log is configured.
func TestMarkCompleted_noLogConfigured(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	notePath := filepath.Join(vaultDir, "X.md")
	os.WriteFile(notePath, []byte("---\ntags:\n  - \"#LightNovel\"\n---\n"), 0o644)
	id, _ := st.UpsertBook("X", "https://x/1", notePath, 1, "", "", nil, "ln", "")
	rec := do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", `{"volume":"1","unknown":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Reading tab (#64): queue an LN volume, start another, reorder the queue,
// promote a queued item to reading, and complete a reading item — which must
// append the log row AND clear the item out of currently-reading.
func TestReadingQueueFlow(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	os.WriteFile(logPath, []byte("| 202506 | [[Seed]] | 1 |  |  |\n| --- | --- | ---: | --- | --- |\n"), 0o644)
	st.SetSetting("reading_log_path", logPath)

	notePath := filepath.Join(vaultDir, "Hell Mode.md")
	os.WriteFile(notePath, []byte("---\ntags:\n  - \"#LightNovel\"\nStatus:\n  - Backlog\n---\n"), 0o644)
	id, _ := st.UpsertBook("Hell Mode", "https://x/hm", notePath, 12, "", "Backlog", nil, "ln", "")

	// Queue volume 4, then queue volume 5.
	if rec := do(h, "POST", "/api/reading/queue", "secret", fmt.Sprintf(`{"book_id":%d,"volume":"4"}`, id)); rec.Code != http.StatusOK {
		t.Fatalf("queue v4: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/api/reading/queue", "secret", fmt.Sprintf(`{"book_id":%d,"volume":"5"}`, id)); rec.Code != http.StatusOK {
		t.Fatalf("queue v5: got %d: %s", rec.Code, rec.Body.String())
	}

	// State shows two queued, nothing reading.
	state := readingState(t, h)
	if len(state.Queue) != 2 || len(state.Reading) != 0 {
		t.Fatalf("after queueing: reading=%d queue=%d", len(state.Reading), len(state.Queue))
	}
	v4, v5 := state.Queue[0].ID, state.Queue[1].ID
	if state.Queue[0].Volume != "4" || state.Queue[1].Volume != "5" {
		t.Fatalf("queue order: %q, %q", state.Queue[0].Volume, state.Queue[1].Volume)
	}

	// Reorder: v5 first.
	if rec := do(h, "PUT", "/api/reading/queue", "secret", fmt.Sprintf(`{"ids":[%d,%d]}`, v5, v4)); rec.Code != http.StatusOK {
		t.Fatalf("reorder: got %d: %s", rec.Code, rec.Body.String())
	}
	if state = readingState(t, h); state.Queue[0].ID != v5 {
		t.Errorf("reorder not persisted: front is %d, want %d", state.Queue[0].ID, v5)
	}

	// Promote the (now-front) v5 into currently-reading.
	if rec := do(h, "POST", fmt.Sprintf("/api/reading/%d/start", v5), "secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("start queued: got %d: %s", rec.Code, rec.Body.String())
	}
	state = readingState(t, h)
	if len(state.Reading) != 1 || len(state.Queue) != 1 {
		t.Fatalf("after promote: reading=%d queue=%d", len(state.Reading), len(state.Queue))
	}
	readingItem := state.Reading[0]
	if readingItem.StartDate == "" {
		t.Error("start date not stamped on promote")
	}

	// ✓ Done on the reading item: logs the read and clears the item.
	body := fmt.Sprintf(`{"volume":%q,"reading_item_id":%d,"unknown":true}`, readingItem.Volume, readingItem.ID)
	if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", body); rec.Code != http.StatusOK {
		t.Fatalf("complete: got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(mustRead(t, logPath)), "[[Hell Mode]] | 5 |") {
		t.Errorf("completed row not logged:\n%s", mustRead(t, logPath))
	}
	state = readingState(t, h)
	if len(state.Reading) != 0 {
		t.Errorf("reading item not cleared after completion: %d left", len(state.Reading))
	}

	// Next-volume suggestion: v5 is now logged (plus the seed v1), so next is 6.
	rec := do(h, "GET", fmt.Sprintf("/api/reading/next-volume?book_id=%d", id), "", "")
	var nv struct {
		Volume string `json:"volume"`
	}
	json.Unmarshal(rec.Body.Bytes(), &nv)
	if nv.Volume != "6" {
		t.Errorf("next-volume = %q, want 6", nv.Volume)
	}

	// Completing v5 also auto-queued v6 (#67), alongside the still-untouched v4.
	if state = readingState(t, h); len(state.Queue) != 2 {
		t.Fatalf("after completion: queue=%d, want 2 (v4 + auto-queued v6)", len(state.Queue))
	}

	// Remove the original queued item; the auto-queued v6 remains.
	if rec := do(h, "DELETE", fmt.Sprintf("/api/reading/%d", v4), "secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d: %s", rec.Code, rec.Body.String())
	}
	if state = readingState(t, h); len(state.Queue) != 1 || state.Queue[0].Volume != "6" {
		t.Fatalf("after delete: queue=%v, want just the auto-queued v6", state.Queue)
	}
}

// Next-volume suggestion (issue #68): a series read before the app existed
// has Read Volumes set but no rows in _Read.md — the suggestion must fall
// back to Read Volumes+1, not default to 1.
func TestReadingNextVolume_seedsFromReadVolumes(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	os.WriteFile(logPath, []byte("| --- | --- | ---: | --- | --- |\n"), 0o644)
	st.SetSetting("reading_log_path", logPath)

	notePath := filepath.Join(vaultDir, "Seirei Gensouki.md")
	os.WriteFile(notePath, []byte("---\ntags:\n  - \"#LightNovel\"\nStatus:\n  - Backlog\n---\n"), 0o644)
	rv := 22
	id, _ := st.UpsertBook("Seirei Gensouki", "https://x/sg", notePath, 27, "", "Backlog", &rv, "ln", "")

	rec := do(h, "GET", fmt.Sprintf("/api/reading/next-volume?book_id=%d", id), "", "")
	var nv struct {
		Volume string `json:"volume"`
	}
	json.Unmarshal(rec.Body.Bytes(), &nv)
	if nv.Volume != "23" {
		t.Errorf("next-volume = %q, want 23", nv.Volume)
	}
}

// When both the log and Read Volumes have data, the suggestion takes
// whichever is higher.
func TestReadingNextVolume_maxOfLogAndReadVolumes(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	os.WriteFile(logPath, []byte("| 202506 | [[Overlord]] | 5 |  |  |\n| --- | --- | ---: | --- | --- |\n"), 0o644)
	st.SetSetting("reading_log_path", logPath)

	notePath := filepath.Join(vaultDir, "Overlord.md")
	os.WriteFile(notePath, []byte("---\ntags:\n  - \"#LightNovel\"\nStatus:\n  - Backlog\n---\n"), 0o644)
	rv := 3
	id, _ := st.UpsertBook("Overlord", "https://x/ol", notePath, 20, "", "Backlog", &rv, "ln", "")

	rec := do(h, "GET", fmt.Sprintf("/api/reading/next-volume?book_id=%d", id), "", "")
	var nv struct {
		Volume string `json:"volume"`
	}
	json.Unmarshal(rec.Body.Bytes(), &nv)
	if nv.Volume != "6" {
		t.Errorf("next-volume = %q, want 6", nv.Volume)
	}
}

type readingStatePayload struct {
	Reading []store.ReadingItem `json:"reading"`
	Queue   []store.ReadingItem `json:"queue"`
}

func readingState(t *testing.T, h http.Handler) readingStatePayload {
	t.Helper()
	rec := do(h, "GET", "/api/reading/queue", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reading state: got %d: %s", rec.Code, rec.Body.String())
	}
	var p readingStatePayload
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Starting a read accepts an optional earlier start date (the user may have
// begun before opening the app); a blank one defaults to today.
func TestReadingStart_optionalStartDate(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	notePath := filepath.Join(vaultDir, "Hell Mode.md")
	os.WriteFile(notePath, []byte("---\ntags:\n  - \"#LightNovel\"\n---\n"), 0o644)
	id, _ := st.UpsertBook("Hell Mode", "https://x/hm", notePath, 12, "", "Backlog", nil, "ln", "")

	if rec := do(h, "POST", "/api/reading/start", "secret", fmt.Sprintf(`{"book_id":%d,"volume":"2","start_date":"2026-06-01"}`, id)); rec.Code != http.StatusOK {
		t.Fatalf("start: got %d: %s", rec.Code, rec.Body.String())
	}
	state := readingState(t, h)
	if len(state.Reading) != 1 || state.Reading[0].StartDate != "2026-06-01" {
		t.Errorf("supplied start date not honored: %+v", state.Reading)
	}
}

// Abandoning a currently-reading item (#64): logs a `----` end marker with the
// start date kept, clears the item, and — for a #Book — sets Status to Dropped.
func TestReadingAbandon(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	os.WriteFile(logPath, []byte("| 202506 | [[Seed]] | 1 |  |  |\n| --- | --- | ---: | --- | --- |\n"), 0o644)
	st.SetSetting("reading_log_path", logPath)

	title, link := "Dropped Book", "https://openlibrary.org/works/OL8W"
	notePath := filepath.Join(vaultDir, title+".md")
	os.WriteFile(notePath, []byte(notes.BuildBookNote(title, "Auth", link, "OL8W", "", "Backlog", "", "", "2026-07-05")), 0o644)
	id, _ := st.UpsertBook(title, link, notePath, 0, "", "Backlog", nil, "book", "Auth")

	// Start it, then abandon with a kept start date.
	rec := do(h, "POST", "/api/reading/start", "secret", fmt.Sprintf(`{"book_id":%d}`, id))
	if rec.Code != http.StatusOK {
		t.Fatalf("start: got %d: %s", rec.Code, rec.Body.String())
	}
	itemID := readingState(t, h).Reading[0].ID
	body := fmt.Sprintf(`{"abandoned":true,"start":"2026-06-15","reading_item_id":%d}`, itemID)
	if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/complete", id), "secret", body); rec.Code != http.StatusOK {
		t.Fatalf("abandon: got %d: %s", rec.Code, rec.Body.String())
	}

	// The log row carries the `----` end marker and the start date.
	raw := string(mustRead(t, logPath))
	if !strings.Contains(raw, "[[Dropped Book]] |  | 2026.06.15 | ---- |") {
		t.Errorf("abandoned row not written as expected:\n%s", raw)
	}
	// Item cleared, #Book set to Dropped.
	if len(readingState(t, h).Reading) != 0 {
		t.Error("reading item not cleared after abandon")
	}
	books, _ := st.ListBooks()
	if len(books) != 1 || books[0].Status != "Dropped" {
		t.Errorf("book not set to Dropped: %+v", books)
	}
	// The completed feed reports it as abandoned.
	var feed struct {
		Reads []struct {
			Title     string `json:"title"`
			Abandoned bool   `json:"abandoned"`
		} `json:"reads"`
	}
	json.Unmarshal(do(h, "GET", "/api/reading/completed", "", "").Body.Bytes(), &feed)
	var found bool
	for _, rd := range feed.Reads {
		if rd.Title == "Dropped Book" && rd.Abandoned {
			found = true
		}
	}
	if !found {
		t.Errorf("abandoned read not surfaced in feed: %+v", feed.Reads)
	}
}

// Editing and deleting a completed-log row over the API (#64).
func TestReadingCompletedEditDelete(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	logPath := filepath.Join(vaultDir, "_Read.md")
	os.WriteFile(logPath, []byte(
		"| 202506 | [[Alpha]] | 1 | 2025.05.01 | 2025.05.10 |\n"+
			"| --- | --- | ---: | --- | --- |\n"+
			"| 202507 | [[Beta]] | 2 | 2025.07.01 | 2025.07.05 |\n"), 0o644)
	st.SetSetting("reading_log_path", logPath)

	// Edit row 1 (Beta) → volume 9, abandoned.
	edit := `{"index":1,"title":"Beta","volume":"9","abandoned":true,"start":"2025-07-01"}`
	if rec := do(h, "PUT", "/api/reading/completed", "secret", edit); rec.Code != http.StatusOK {
		t.Fatalf("edit: got %d: %s", rec.Code, rec.Body.String())
	}
	raw := string(mustRead(t, logPath))
	if !strings.Contains(raw, "[[Beta]] | 9 | 2025.07.01 | ---- |") {
		t.Errorf("edit not applied:\n%s", raw)
	}

	// A stale/mismatched title is a 409, not a wrong-row edit.
	if rec := do(h, "PUT", "/api/reading/completed", "secret", `{"index":1,"title":"Ghost","volume":"1"}`); rec.Code != http.StatusConflict {
		t.Errorf("title mismatch: got %d, want 409", rec.Code)
	}

	// Delete row 0 (Alpha).
	if rec := do(h, "DELETE", "/api/reading/completed?index=0&title=Alpha", "secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d: %s", rec.Code, rec.Body.String())
	}
	feed := do(h, "GET", "/api/reading/completed", "", "")
	var got struct {
		Reads []struct {
			Title string `json:"title"`
		} `json:"reads"`
	}
	json.Unmarshal(feed.Body.Bytes(), &got)
	if len(got.Reads) != 1 || got.Reads[0].Title != "Beta" {
		t.Errorf("after delete, feed = %+v", got.Reads)
	}
}

// A whole #Book is queued/started with no volume, whatever the client sends.
func TestReadingStart_bookIgnoresVolume(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	notePath := filepath.Join(vaultDir, "A Novel.md")
	os.WriteFile(notePath, []byte(notes.BuildBookNote("A Novel", "Auth", "https://openlibrary.org/works/OL5W", "OL5W", "", "Backlog", "", "", "2026-07-05")), 0o644)
	id, _ := st.UpsertBook("A Novel", "https://openlibrary.org/works/OL5W", notePath, 0, "", "Backlog", nil, "book", "Auth")

	if rec := do(h, "POST", "/api/reading/start", "secret", fmt.Sprintf(`{"book_id":%d,"volume":"7"}`, id)); rec.Code != http.StatusOK {
		t.Fatalf("start book: got %d: %s", rec.Code, rec.Body.String())
	}
	state := readingState(t, h)
	if len(state.Reading) != 1 || state.Reading[0].Volume != "" {
		t.Errorf("book reading item should carry no volume: %+v", state.Reading)
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

// Hard-deleting an LN series also wipes its whole _volumes/<Series>/ folder and
// each volume note's cover attachment (#100). The soft untrack path leaves them.
func TestDeleteBook_hardWipesVolumeNotesAndCovers(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	attach := filepath.Join(vaultDir, "_attachments")
	if err := os.MkdirAll(attach, 0o755); err != nil {
		t.Fatal(err)
	}
	series := "Kumo Desu ga Nani ka"
	seriesCover := notes.CoverName(series, ".jpg")
	seriesNote := filepath.Join(vaultDir, series+".md")
	if err := os.WriteFile(seriesNote, []byte("---\ntags:\n  - \"#LightNovel\"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attach, seriesCover), []byte("SERIES"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Volume 1 has a cover; volume 2 is a hand-edited placeholder with none.
	volDir := notes.VolumeDir(seriesNote)
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vol1Cover := filepath.Join(attach, "cover_kumo_vol1.jpg")
	if err := os.WriteFile(vol1Cover, []byte("VOL1"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol1 := notes.BuildLNVolumeNote(series, 1, 2, "eng", "https://jnovels.com/k1/", "cover_kumo_vol1.jpg", "", "Blurb.", "2026-07-17", false)
	if err := os.WriteFile(filepath.Join(volDir, "Kumo Desu ga Nani ka Volume 1.md"), []byte(vol1), 0o644); err != nil {
		t.Fatal(err)
	}
	vol2 := notes.BuildLNVolumeNote(series, 2, 2, "eng", "", "", "", "", "2026-07-17", true)
	if err := os.WriteFile(filepath.Join(volDir, "Kumo Desu ga Nani ka Volume 2.md"), []byte(vol2), 0o644); err != nil {
		t.Fatal(err)
	}

	id, _ := st.UpsertBook(series, "https://x/kumo", seriesNote, 2, seriesCover, "Backlog", nil, "ln", "")

	if rec := do(h, "DELETE", fmt.Sprintf("/api/books/%d?hard=1", id), "secret", ""); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(volDir); !os.IsNotExist(err) {
		t.Errorf("_volumes/<Series>/ folder not wiped: %v", err)
	}
	if _, err := os.Stat(vol1Cover); !os.IsNotExist(err) {
		t.Errorf("volume cover not deleted: %v", err)
	}
	if _, err := os.Stat(seriesNote); !os.IsNotExist(err) {
		t.Errorf("series note not deleted: %v", err)
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

// TestLCSearchMissingQ: the Lubimyczytać fallback search validates its query,
// same as the OL proxy (#60).
func TestLCSearchMissingQ(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "GET", "/api/lc/search", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing q: got %d, want 400", rec.Code)
	}
}

// TestJnovelsSearchMissingQ: the jnovels light-novel fallback validates its
// query, same as the OL and LC proxies (#89).
func TestJnovelsSearchMissingQ(t *testing.T) {
	h, _, _ := newTestServer(t)
	if rec := do(h, "GET", "/api/jnovels/search", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing q: got %d, want 400", rec.Code)
	}
}

// TestAddBookLCValidation: the LC add path requires title, work_id and the LC
// link (ol_url) before any network work — a missing field is a 400 (#60).
func TestAddBookLCValidation(t *testing.T) {
	h, _, _ := newTestServer(t)
	body := `{"kind":"book","source":"lc","title":"Prawa i Powinności"}` // no work_id/ol_url
	rec := do(h, "POST", "/api/books", "secret", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("incomplete LC add: got %d, want 400", rec.Code)
	}
}

// TestCSPAllowsLCCovers: Lubimyczytać cover host must be in the img-src
// whitelist, or the picker/preview can't render Polish covers (#60).
func TestCSPAllowsLCCovers(t *testing.T) {
	h, _, _ := newTestServer(t)
	csp := do(h, "GET", "/", "", "").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "lubimyczytac.pl") {
		t.Errorf("CSP img-src missing lubimyczytac.pl: %q", csp)
	}
}

// The reading tile for an LN volume requests that volume's own cover via ?vol=N
// (#90): the server serves the #LNVolume note's cover, and falls back to the
// series cover when the volume note is missing or coverless.
func TestCover_volumeOverride(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	attach := filepath.Join(vaultDir, "_attachments")
	if err := os.MkdirAll(attach, 0o755); err != nil {
		t.Fatal(err)
	}
	// Distinct bytes so we can tell which cover was served.
	if err := os.WriteFile(filepath.Join(attach, "cover_series.jpg"), []byte("SERIES-COVER"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attach, "cover_vol3.jpg"), []byte("VOLUME-3-COVER"), 0o644); err != nil {
		t.Fatal(err)
	}

	seriesNote := filepath.Join(vaultDir, "Kumo Desu ga Nani ka.md")
	if err := os.WriteFile(seriesNote, []byte("---\nSeries: Kumo Desu ga Nani ka\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One resolved volume note (index 3, has a cover) under _volumes/<Series>/.
	volDir := notes.VolumeDir(seriesNote)
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vol3 := notes.BuildLNVolumeNote("Kumo Desu ga Nani ka", 3, 10, "eng", "https://jnovels.com/kumo-vol-3-epub/", "cover_vol3.jpg", "", "Blurb.", "2026-07-17", false)
	if err := os.WriteFile(filepath.Join(volDir, "Kumo Desu ga Nani ka Volume 3.md"), []byte(vol3), 0o644); err != nil {
		t.Fatal(err)
	}

	id, _ := st.UpsertBook("Kumo Desu ga Nani ka", "https://x/kumo", seriesNote, 10, "cover_series.jpg", "Backlog", nil, "ln", "")

	// ?vol=3 → the volume's own cover.
	if rec := do(h, "GET", fmt.Sprintf("/api/cover/%d?vol=3", id), "", ""); rec.Body.String() != "VOLUME-3-COVER" {
		t.Errorf("?vol=3 served %q, want the volume-3 cover", rec.Body.String())
	}
	// ?vol=7 (no such volume note) → falls back to the series cover.
	if rec := do(h, "GET", fmt.Sprintf("/api/cover/%d?vol=7", id), "", ""); rec.Body.String() != "SERIES-COVER" {
		t.Errorf("?vol=7 served %q, want the series cover fallback", rec.Body.String())
	}
	// No ?vol → the series cover.
	if rec := do(h, "GET", fmt.Sprintf("/api/cover/%d", id), "", ""); rec.Body.String() != "SERIES-COVER" {
		t.Errorf("no vol served %q, want the series cover", rec.Body.String())
	}
}

// The volume-status endpoint reports resolved/incomplete/missing per volume so
// the note-modal reviewer can list what still needs filling (#90 phase 2).
func TestVolumeStatesEndpoint(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	series := "Kumo Desu ga Nani ka"
	seriesNote := filepath.Join(vaultDir, series+".md")
	if err := os.WriteFile(seriesNote, []byte("---\ntags:\n  - \"#LightNovel\"\nVolumes: 3\n---\n### "+series+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := notes.Options{VaultDir: vaultDir, NewNoteDir: "", AttachmentsDir: "_attachments"}
	if _, err := notes.CreateLNVolume(o, series, 1, 3, "eng", "https://jnovels.com/k1/", "", "", "Blurb.", false); err != nil {
		t.Fatal(err)
	}
	if _, err := notes.CreateLNVolume(o, series, 2, 3, "eng", "", "", "", "", true); err != nil {
		t.Fatal(err)
	}
	id, _ := st.UpsertBook(series, "https://jnovels.com/kumo/", seriesNote, 3, "", "Backlog", nil, "ln", "")

	rec := do(h, "GET", fmt.Sprintf("/api/books/%d/volumes", id), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET volumes: %d", rec.Code)
	}
	var out struct {
		Volumes int `json:"volumes"`
		States  []struct {
			Volume int    `json:"volume"`
			State  string `json:"state"`
		} `json:"states"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Volumes != 3 || len(out.States) != 3 {
		t.Fatalf("got volumes=%d states=%d, want 3/3", out.Volumes, len(out.States))
	}
	want := []string{"resolved", "incomplete", "missing"}
	for i, w := range want {
		if out.States[i].State != w {
			t.Errorf("volume %d = %q, want %q", i+1, out.States[i].State, w)
		}
	}
}

// The backfill + resolve endpoints reject non-LN books and bad input before any
// network work.
func TestBackfillEndpoints_guards(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	bookNote := filepath.Join(vaultDir, "A Regular Book.md")
	os.WriteFile(bookNote, []byte("---\ntags:\n  - \"#Book\"\n---\n"), 0o644)
	bookID, _ := st.UpsertBook("A Regular Book", "https://x/1", bookNote, 0, "", "Backlog", nil, "book", "Auth")

	// Non-LN → 400.
	if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/backfill-volumes", bookID), "secret", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("backfill on a #Book: got %d, want 400", rec.Code)
	}
	// Resolve with a bad URL → 400 (before any scrape).
	if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/volumes/1/resolve", bookID), "secret", `{"url":"not a url"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("resolve bad url: got %d, want 400", rec.Code)
	}
	// Write routes need auth.
	if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/backfill-volumes", bookID), "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("backfill without token: got %d, want 401", rec.Code)
	}
}

// Manual fill writes a resolved volume note from typed values (the reviewer's
// fallback for a volume that isn't on jnovels), and the backfill-status endpoint
// then drops it from the attention counts.
func TestFillVolumeManual_andStatus(t *testing.T) {
	h, st, vaultDir := newTestServer(t)
	series := "Test Series"
	seriesNote := filepath.Join(vaultDir, series+".md")
	os.WriteFile(seriesNote, []byte("---\ntags:\n  - \"#LightNovel\"\nVolumes: 2\n---\n### "+series+"\n"), 0o644)
	o := notes.Options{VaultDir: vaultDir, NewNoteDir: "", AttachmentsDir: "_attachments"}
	notes.CreateLNVolume(o, series, 1, 2, "eng", "https://jnovels.com/t1/", "", "", "Blurb.", false) // resolved
	notes.CreateLNVolume(o, series, 2, 2, "eng", "", "", "", "", true)                               // incomplete
	id, _ := st.UpsertBook(series, "https://jnovels.com/t/", seriesNote, 2, "", "Backlog", nil, "ln", "")

	// Before: volume 2 counts toward the attention badge.
	rec := do(h, "GET", "/api/books/backfill-status", "", "")
	var status map[string]int
	json.Unmarshal(rec.Body.Bytes(), &status)
	if status[fmt.Sprint(id)] != 1 {
		t.Fatalf("pre-fill status = %v, want {%d:1}", status, id)
	}

	// Fill volume 2 by hand (description only, no cover).
	rec = do(h, "POST", fmt.Sprintf("/api/books/%d/volumes/2/fill", id), "secret", `{"description":"Hand-written blurb."}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fill: %d — %s", rec.Code, rec.Body.String())
	}
	// The note is now a resolved #LNVolume with the typed description.
	data, _ := os.ReadFile(filepath.Join(vaultDir, "_volumes", series, "Test Series Volume 2.md"))
	if !strings.Contains(string(data), "Hand-written blurb.") || strings.Contains(string(data), "#LNVolume/incomplete") {
		t.Errorf("filled note wrong:\n%s", data)
	}

	// After: the status cache was busted and volume 2 no longer counts.
	rec = do(h, "GET", "/api/books/backfill-status", "", "")
	var after map[string]int
	json.Unmarshal(rec.Body.Bytes(), &after)
	if _, still := after[fmt.Sprint(id)]; still {
		t.Errorf("post-fill status still flags the series: %v", after)
	}

	// Filling an already-resolved volume is refused.
	if rec := do(h, "POST", fmt.Sprintf("/api/books/%d/volumes/1/fill", id), "secret", `{"description":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("fill resolved volume: got %d, want 409", rec.Code)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.0", "1.1.9", 1},
		{"1.1.0", "1.2.0", -1},
		{"1.2.0", "1.2.0", 0},
		{"1.2", "1.2.0", 0},
		{"1.2.1", "1.2", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.2.0-rc1", "1.2.0", 0},
		{"1.10.0", "1.9.0", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
