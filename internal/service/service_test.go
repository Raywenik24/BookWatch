package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bookwatch/internal/scraper"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

func init() { scraper.AllowPrivateHosts = true } // httptest binds to loopback

func novelHTML(vols int) string {
	var b strings.Builder
	for i := 1; i <= vols; i++ {
		fmt.Fprintf(&b, "<li>Download VOLUME %d Epub</li>", i)
	}
	return `<!doctype html><html><body>
<h1 class="post-title entry-title">N Epub</h1>
<div class="featured-media"><img src="/c.jpg"></div>
<div class="synopsis-description"><p>D.</p></div>
<ol>` + b.String() + `</ol></body></html>`
}

func writeNote(t *testing.T, dir, name, link string, vols int) string {
	t.Helper()
	content := fmt.Sprintf("---\nLink: %s\nVolumes: %d\ntags:\n  - \"#LightNovel\"\nTemplate_used: LightNovelTemplate\n---\n### %s\n", link, vols, name)
	p := filepath.Join(dir, name+".md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRunCheck_detectOnlyThenApply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(novelHTML(3)))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	notePath := writeNote(t, vaultDir, "Book A", srv.URL+"/a", 2)
	st := openStore(t)
	sc := scraper.New("t", 5*time.Second)

	// Detect-only: finds the bump but writes nothing.
	sum, err := RunCheck(sc, st, vaultDir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Checked != 1 || sum.Updated != 1 || sum.Errors != 0 {
		t.Fatalf("summary: %+v", sum)
	}
	if raw, _ := os.ReadFile(notePath); !strings.Contains(string(raw), "Volumes: 2") {
		t.Errorf("detect-only must not write the vault:\n%s", raw)
	}
	if n, _ := st.CountPending(); n != 1 {
		t.Errorf("expected 1 pending update, got %d", n)
	}
	if books, _ := st.ListBooks(); books[0].Volumes != 2 {
		t.Errorf("detect-only must not bump the book: %d", books[0].Volumes)
	}

	// Apply writes the stored number to the note and bumps the book.
	res, err := ApplyPending(st, vault.Today())
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 1 || res.Failed != 0 {
		t.Fatalf("apply: %+v", res)
	}
	if raw, _ := os.ReadFile(notePath); !strings.Contains(string(raw), "Volumes: 3") {
		t.Errorf("apply should write Volumes: 3:\n%s", raw)
	}
	if books, _ := st.ListBooks(); books[0].Volumes != 3 {
		t.Errorf("book not bumped after apply: %d", books[0].Volumes)
	}
	if n, _ := st.CountPending(); n != 0 {
		t.Errorf("pending not cleared after apply: %d", n)
	}
}

func TestRunCheck_logsScrapeAnomaly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 but no volume list → count 0 → suspicious, not an error.
		w.Write([]byte(`<html><body><h1 class="post-title entry-title">N</h1></body></html>`))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	writeNote(t, vaultDir, "Broken", srv.URL+"/x", 4)
	st := openStore(t)
	sc := scraper.New("t", 5*time.Second)

	sum, err := RunCheck(sc, st, vaultDir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Suspicious != 1 || sum.Updated != 0 || sum.Errors != 0 {
		t.Fatalf("expected 1 suspicious / 0 updates / 0 errors: %+v", sum)
	}
	evs, _ := st.ListEvents(10)
	found := false
	for _, e := range evs {
		if e.Kind == "anomaly" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an anomaly event to be logged, got %+v", evs)
	}
	// The bad read must not change the recorded volume count.
	if books, _ := st.ListBooks(); len(books) != 1 || books[0].Volumes != 4 {
		t.Errorf("suspicious scrape must not change stored volumes: %+v", books)
	}
}

func TestRunCheck_prunesOnlyMissingNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(novelHTML(1)))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	writeNote(t, vaultDir, "Keep", srv.URL+"/keep", 1) // scanned + kept
	st := openStore(t)
	sc := scraper.New("t", 5*time.Second)

	// Stale: not in the scan (note gone) → must be pruned.
	st.UpsertBook("Stale", "https://nope/x", filepath.Join(vaultDir, "gone.md"), 1, "", "", nil)
	// Not in the scan (note exists but lacks Template_used filter) → also pruned.
	existing := filepath.Join(vaultDir, "untagged.md")
	os.WriteFile(existing, []byte("not a LN note"), 0o644)
	st.UpsertBook("OnDisk", "https://nope/y", existing, 1, "", "", nil)

	if _, err := RunCheck(sc, st, vaultDir, false, nil); err != nil {
		t.Fatal(err)
	}

	links := map[string]bool{}
	books, _ := st.ListBooks()
	for _, b := range books {
		links[b.Link] = true
	}
	if links["https://nope/x"] {
		t.Error("stale book with a missing note should have been pruned")
	}
	if links["https://nope/y"] {
		t.Error("book whose note lacks Template_used should have been pruned")
	}
}
