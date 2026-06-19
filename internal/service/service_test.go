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
	content := fmt.Sprintf("---\nLink: %s\nVolumes: %d\ntags:\n  - \"#LightNovel\"\n---\n### %s\n", link, vols, name)
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

func TestRunCheck_prunesOnlyMissingNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(novelHTML(1)))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	writeNote(t, vaultDir, "Keep", srv.URL+"/keep", 1) // scanned + kept
	st := openStore(t)
	sc := scraper.New("t", 5*time.Second)

	// Stale: not in the scan AND its note path is gone → must be pruned.
	st.UpsertBook("Stale", "https://nope/x", filepath.Join(vaultDir, "gone.md"), 1, "")
	// Not in the scan but its path still exists on disk → must be kept
	// (guards against a transient stat failure wrongly pruning).
	existing := filepath.Join(vaultDir, "untagged.md")
	os.WriteFile(existing, []byte("not a LN note"), 0o644)
	st.UpsertBook("OnDisk", "https://nope/y", existing, 1, "")

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
	if !links["https://nope/y"] {
		t.Error("book whose note still exists on disk must be kept")
	}
}
