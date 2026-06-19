package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bookwatch/internal/config"
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
	return New(cfg, st, sc, sched).Handler(), st, vaultDir
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
	st.UpsertBook("A", "https://x/1", "", 2, "")

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

func TestDeleteBook_untracksAndLogsEvent(t *testing.T) {
	h, st, _ := newTestServer(t)
	id, _ := st.UpsertBook("A", "https://x/1", "", 2, "")

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
	id, _ := st.UpsertBook("Late", "https://x/late", "", 1, "late.png")

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
	noCover, _ := st.UpsertBook("A", "https://x/1", "", 1, "")
	if rec := do(h, "GET", fmt.Sprintf("/api/cover/%d", noCover), "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("no-cover book: got %d", rec.Code)
	}

	// Real cover in the attachments dir → 200 with the bytes.
	attach := filepath.Join(vaultDir, "_attachments")
	os.MkdirAll(attach, 0o755)
	os.WriteFile(filepath.Join(attach, "c.png"), []byte("PNGDATA"), 0o644)
	withCover, _ := st.UpsertBook("B", "https://x/2", "", 1, "c.png")
	rec := do(h, "GET", fmt.Sprintf("/api/cover/%d", withCover), "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "PNGDATA" {
		t.Errorf("cover serve: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// A traversal-style cover value is reduced to its basename (filepath.Base),
	// so it cannot escape the vault; the basename isn't present → 404, not a leak.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("TOPSECRET"), 0o644)
	evil, _ := st.UpsertBook("C", "https://x/3", "", 1, "../../../"+secret)
	rec = do(h, "GET", fmt.Sprintf("/api/cover/%d", evil), "", "")
	if rec.Body.String() == "TOPSECRET" {
		t.Error("path traversal escaped the vault — cover guard failed")
	}
}
