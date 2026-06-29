package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newGBTestServer returns a GBClient pointed at an httptest server whose
// handler is provided by the caller. (scraper.AllowPrivateHosts is enabled in
// openlibrary_test.go's init, which also runs for this package.)
func newGBTestServer(t *testing.T, h http.HandlerFunc) (*GBClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := NewGoogleBooks("", 5*time.Second)
	c.baseURL = srv.URL
	return c, srv
}

func TestGBCoverURLHit(t *testing.T) {
	var gotQuery string
	c, srv := newGBTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		// GB returns http:// thumbnail URLs; the client must force https.
		w.Write([]byte(`{"items":[{"volumeInfo":{"imageLinks":{"thumbnail":"http://books.google.com/books/content?id=abc"}}}]}`))
	})
	defer srv.Close()

	got := c.CoverURL("Night Plague", "Graham Masterton")
	if got != "https://books.google.com/books/content?id=abc" {
		t.Errorf("cover = %q, want https rewrite of thumbnail", got)
	}
	// Phrases must be quoted so the qualifier binds to the whole title/author.
	if !strings.Contains(gotQuery, `intitle:"Night Plague"`) || !strings.Contains(gotQuery, `inauthor:"Graham Masterton"`) {
		t.Errorf("query %q should use quoted intitle/inauthor phrases", gotQuery)
	}
}

func TestGBCoverURLNoAuthor(t *testing.T) {
	var gotQuery string
	c, srv := newGBTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Write([]byte(`{"items":[{"volumeInfo":{"imageLinks":{"thumbnail":"https://books.google.com/x"}}}]}`))
	})
	defer srv.Close()

	if got := c.CoverURL("Some Title", ""); got != "https://books.google.com/x" {
		t.Errorf("cover = %q", got)
	}
	if strings.Contains(gotQuery, "inauthor") {
		t.Errorf("query %q should omit inauthor when author empty", gotQuery)
	}
}

// The top GB hit often lacks a thumbnail even when a later edition has one;
// CoverURL must scan past image-less items rather than give up on the first.
func TestGBCoverURLSkipsImagelessItems(t *testing.T) {
	c, srv := newGBTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[
			{"volumeInfo":{}},
			{"volumeInfo":{"imageLinks":{"smallThumbnail":"http://books.google.com/small"}}}
		]}`))
	})
	defer srv.Close()

	if got := c.CoverURL("Drowned", "Graham Masterton"); got != "https://books.google.com/small" {
		t.Errorf("cover = %q, want second item's smallThumbnail", got)
	}
}

// When the strict intitle pass misses, CoverURL retries with an author-only
// query so editions whose title metadata lacks the exact phrase still resolve.
func TestGBCoverURLLooserFallback(t *testing.T) {
	var queries []string
	c, srv := newGBTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		queries = append(queries, q)
		if strings.Contains(q, "intitle:") {
			w.Write([]byte(`{}`)) // strict pass misses
			return
		}
		w.Write([]byte(`{"items":[{"volumeInfo":{"imageLinks":{"thumbnail":"http://books.google.com/loose"}}}]}`))
	})
	defer srv.Close()

	got := c.CoverURL("Les anges oubliés", "Graham Masterton")
	if got != "https://books.google.com/loose" {
		t.Errorf("cover = %q, want looser-pass thumbnail", got)
	}
	if len(queries) != 2 {
		t.Fatalf("want strict + looser passes (2 requests), got %d: %v", len(queries), queries)
	}
	if strings.Contains(queries[1], "intitle:") {
		t.Errorf("looser pass %q should drop intitle", queries[1])
	}
	if !strings.Contains(queries[1], `inauthor:"Graham Masterton"`) {
		t.Errorf("looser pass %q should keep author constraint", queries[1])
	}
}

func TestGBCoverURLMiss(t *testing.T) {
	c, srv := newGBTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`)) // no items, every pass
	})
	defer srv.Close()

	if got := c.CoverURL("Nonexistent", "Nobody"); got != "" {
		t.Errorf("miss should yield empty string, got %q", got)
	}
}

func TestGBCoverURLErrorStatus(t *testing.T) {
	c, srv := newGBTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	})
	defer srv.Close()

	if got := c.CoverURL("Anything", "Anyone"); got != "" {
		t.Errorf("non-200 should yield empty string, got %q", got)
	}
}

func TestGBCoverURLKeyPassthrough(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		w.Write([]byte(`{"items":[{"volumeInfo":{"imageLinks":{"thumbnail":"https://books.google.com/x"}}}]}`))
	}))
	defer srv.Close()

	c := NewGoogleBooks("secret-key", 5*time.Second)
	c.baseURL = srv.URL
	c.CoverURL("Title", "Author")
	if gotKey != "secret-key" {
		t.Errorf("key = %q, want it carried on the request", gotKey)
	}
}
