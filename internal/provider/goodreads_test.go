package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- fixtures (trimmed live Goodreads book-page markup) ---

// grBook builds a book page carrying the work id (the shared dedup key), an
// Open Graph cover/title, and a JSON-LD author — exactly the fields parseGRBook
// reads, in the shapes the live site serves them.
func grBook(workID, title, author, cover string) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><head>
<meta property="og:title" content="%s"/>
<meta property="og:image" content="%s"/>
<script type="application/ld+json">{"@type":"Book","name":"%s","author":[{"@type":"Person","name":"%s"}]}</script>
</head><body>
<a class="ContributorLink" href="/author/show/1405152.%s"><span class="ContributorLink__name">%s</span></a>
<a href="/work/editions/%s-slug" class="otherEditions">All Editions</a>
</body></html>`, title, cover, title, author,
		strings.ReplaceAll(author, " ", "_"), author, workID)
}

// The Demon Cycle book 1 cluster: English, French and Spanish editions all carry
// Goodreads work 6589794; the Polish edition is a *separate* Goodreads work
// (21446513), as the live site genuinely files it. A "junk" ISBN resolves to a
// different author's book, to exercise the dirty-ISBN guard.
func newGRTestServer(t *testing.T) (*GRClient, *httptest.Server) {
	t.Helper()
	books := map[string]string{ // isbn -> book id
		"9780345518705": "6993490", // English Warded Man
		"9782352944928": "6993491", // French L'homme rune
		"9788445077443": "6993492", // Spanish El hombre marcado
		"8375740578":    "5500001", // Polish Malowany czlowiek (separate work)
		"072786596X":    "3360681", // junk: OL listed it under "Painted Man" but it's another book
	}
	pages := map[string]string{
		"6993490": grBook("6589794", "The Warded Man (Demon Cycle, #1)", "Peter V. Brett", "https://m.media-amazon.com/books/6993490._SY475_.jpg"),
		"6993491": grBook("6589794", "Le Cycle des démons, T1", "Peter V. Brett", "https://m.media-amazon.com/books/6993491.jpg"),
		"6993492": grBook("6589794", "El hombre marcado", "Peter V. Brett", "https://m.media-amazon.com/books/nophoto/x.png"),
		"5500001": grBook("21446513", "Malowany człowiek. Księga I", "Peter V. Brett", "https://m.media-amazon.com/books/5500001.jpg"),
		"3360681": grBook("3360681", "The Painted Man (Sissy Sawyer, #2)", "Jane Doe", "https://m.media-amazon.com/books/3360681.jpg"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch {
		case strings.HasPrefix(r.URL.Path, "/book/isbn/"):
			isbn := strings.TrimPrefix(r.URL.Path, "/book/isbn/")
			if id, ok := books[isbn]; ok {
				http.Redirect(w, r, "/book/show/"+id, http.StatusMovedPermanently)
				return
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/book/show/"):
			id := strings.TrimPrefix(r.URL.Path, "/book/show/")
			if html, ok := pages[id]; ok {
				w.Write([]byte(html))
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	c := NewGoodreads("bookwatch-test/1.0", 5*time.Second)
	c.baseURL = srv.URL
	c.minGap = 0
	return c, srv
}

func TestGRMatchWork(t *testing.T) {
	c, srv := newGRTestServer(t)
	defer srv.Close()

	m := c.MatchWork("The Warded Man", "Peter V. Brett", []string{"9780345518705"})
	if !m.Found {
		t.Fatal("expected a match")
	}
	if m.WorkID != "6589794" {
		t.Errorf("work id %q, want 6589794 (the shared cluster id)", m.WorkID)
	}
	if m.Title != "The Warded Man" {
		t.Errorf("title %q — series suffix should be stripped", m.Title)
	}
	if strings.Contains(m.CoverURL, "_SY475_") || !strings.Contains(m.CoverURL, "6993490") {
		t.Errorf("cover %q — should be full-res (size token stripped)", m.CoverURL)
	}
}

func TestGRMatchWorkClustersTranslations(t *testing.T) {
	c, srv := newGRTestServer(t)
	defer srv.Close()

	en := c.MatchWork("The Warded Man", "Peter V. Brett", []string{"9780345518705"})
	fr := c.MatchWork("Le Cycle des démons", "Peter V. Brett", []string{"9782352944928"})
	es := c.MatchWork("El hombre marcado", "Peter V. Brett", []string{"9788445077443"})
	if en.WorkID != fr.WorkID || en.WorkID != es.WorkID {
		t.Errorf("EN/FR/ES must share a work id: en=%q fr=%q es=%q", en.WorkID, fr.WorkID, es.WorkID)
	}
	if es.CoverURL != "" {
		t.Errorf("Spanish nophoto placeholder should yield empty cover, got %q", es.CoverURL)
	}
}

func TestGRMatchWorkPolishStaysSeparate(t *testing.T) {
	c, srv := newGRTestServer(t)
	defer srv.Close()
	en := c.MatchWork("The Warded Man", "Peter V. Brett", []string{"9780345518705"})
	pl := c.MatchWork("Malowany człowiek", "Peter V. Brett", []string{"8375740578"})
	if !pl.Found {
		t.Fatal("polish edition resolves, just to a different work")
	}
	if en.WorkID == pl.WorkID {
		t.Errorf("Goodreads files Polish as a separate work — must NOT share id (%q)", pl.WorkID)
	}
}

func TestGRMatchWorkRejectsDirtyISBN(t *testing.T) {
	c, srv := newGRTestServer(t)
	defer srv.Close()
	// OL listed 072786596X under Brett's "Painted Man", but it resolves to a book
	// by "Jane Doe" — the author guard must reject it (no false cluster).
	m := c.MatchWork("The Painted Man", "Peter V. Brett", []string{"072786596X"})
	if m.Found {
		t.Errorf("dirty ISBN (different author) must be rejected, got work %q", m.WorkID)
	}
}

func TestGRMatchWorkTriesNextISBN(t *testing.T) {
	c, srv := newGRTestServer(t)
	defer srv.Close()
	// First ISBN is dirty (Jane Doe), second is the real English edition — the
	// guard skips the first and accepts the second.
	m := c.MatchWork("The Painted Man", "Peter V. Brett", []string{"072786596X", "9780345518705"})
	if !m.Found || m.WorkID != "6589794" {
		t.Errorf("should fall through to the valid ISBN: found=%v work=%q", m.Found, m.WorkID)
	}
}

func TestGRMatchWorkCache(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if strings.HasPrefix(r.URL.Path, "/book/isbn/") {
			http.Redirect(w, r, "/book/show/6993490", http.StatusMovedPermanently)
			return
		}
		w.Write([]byte(grBook("6589794", "The Warded Man", "Peter V. Brett", "x.jpg")))
	}))
	defer srv.Close()
	c := NewGoodreads("t", 5*time.Second)
	c.baseURL = srv.URL
	c.minGap = 0

	c.MatchWork("x", "Peter V. Brett", []string{"9780345518705"})
	first := hits
	c.MatchWork("x", "Peter V. Brett", []string{"9780345518705"})
	if hits != first {
		t.Errorf("second lookup of the same ISBN should be cached: %d extra request(s)", hits-first)
	}
}

func TestGRCoverByISBN(t *testing.T) {
	c, srv := newGRTestServer(t)
	defer srv.Close()
	if cov := c.CoverByISBN([]string{"9780345518705"}); cov == "" {
		t.Error("expected a cover for the English edition")
	}
	// Spanish edition is a placeholder cover; the English fallback should win.
	if cov := c.CoverByISBN([]string{"9788445077443", "9780345518705"}); !strings.Contains(cov, "6993490") {
		t.Errorf("should skip the placeholder and return the real cover, got %q", cov)
	}
}

func TestGRMiss(t *testing.T) {
	c, srv := newGRTestServer(t)
	defer srv.Close()
	if m := c.MatchWork("Nothing", "Nobody", []string{"0000000000"}); m.Found {
		t.Error("unknown ISBN should miss")
	}
	if m := c.MatchWork("Nothing", "Nobody", nil); m.Found {
		t.Error("no ISBNs should miss")
	}
}

func TestGRWAFChallengeIsAMiss(t *testing.T) {
	// The live /search returns 202 with an empty body; any non-200 must degrade
	// to a miss, never a crash.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	c := NewGoodreads("t", 3*time.Second)
	c.baseURL = srv.URL
	c.minGap = 0
	if m := c.MatchWork("x", "y", []string{"9780345518705"}); m.Found {
		t.Error("a 202 challenge must read as a miss")
	}
}

func TestGRFullCover(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://m.media-amazon.com/books/123._SX98_.jpg", "https://m.media-amazon.com/books/123.jpg"},
		{"https://m.media-amazon.com/books/123._SY475_.jpg", "https://m.media-amazon.com/books/123.jpg"},
		{"https://m.media-amazon.com/books/123.jpg", "https://m.media-amazon.com/books/123.jpg"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := grFullCover(tc.in); got != tc.want {
			t.Errorf("grFullCover(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeISBN(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"978-0-345-51870-5", "9780345518705"},
		{" 0345518705 ", "0345518705"},
		{"072786596X", "072786596X"},
	} {
		if got := normalizeISBN(tc.in); got != tc.want {
			t.Errorf("normalizeISBN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSameAuthor(t *testing.T) {
	if !sameAuthor("Peter V. Brett", "Peter V. Brett") {
		t.Error("identical names should match")
	}
	if !sameAuthor("Peter V. Brett", "Peter Brett") {
		t.Error("surname match should suffice")
	}
	if sameAuthor("Peter V. Brett", "Jane Doe") {
		t.Error("different surnames must not match")
	}
	if sameAuthor("", "Anyone") {
		t.Error("empty name must not match")
	}
}

func TestStripSeries(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"The Warded Man (Demon Cycle, #1)", "The Warded Man"},
		{"Malowany człowiek (Cykl demoniczny, #1)", "Malowany człowiek"},
		{"Standalone Title", "Standalone Title"},
		{"A Book (Not A Series)", "A Book (Not A Series)"},
	} {
		if got := stripSeries(tc.in); got != tc.want {
			t.Errorf("stripSeries(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
