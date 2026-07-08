package scraper

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func loadSearchFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/jnovels_search.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Against saved jnovels search HTML: every hit shares the query tokens, so the
// tiebreaks decide the order — the aggregate "Light Novel" pages rank above the
// single-volume posts, and within a tie epub sorts ahead of pdf. The page-title
// and widget <h1>s (no child <a>) must be skipped.
func TestParseSearchHTML_ranking(t *testing.T) {
	res, err := ParseSearchHTML(loadSearchFixture(t), "Mushoku Tensei: Redundant Reincarnation")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-light-novel-epub/",
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-light-novel-pdf/",
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-3-epub/",
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-2-epub/",
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-3-pdf/",
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-2-pdf/",
	}
	if len(res) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(res), len(want), res)
	}
	for i, w := range want {
		if res[i].URL != w {
			t.Errorf("result[%d] URL = %q, want %q", i, res[i].URL, w)
		}
	}
	if res[0].Title != "Mushoku Tensei: Redundant Reincarnation Light Novel Epub" {
		t.Errorf("top title %q", res[0].Title)
	}
}

func TestParseSearchHTML_noOverlapDropped(t *testing.T) {
	res, err := ParseSearchHTML(loadSearchFixture(t), "Overlord")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("expected no results for an unrelated query, got %+v", res)
	}
}

// A single-novel page's h1.post-title.entry-title holds bare text (no <a>), so
// the search selector must not turn it into a hit; and duplicate URLs collapse.
func TestParseSearchHTML_selectorAndDedup(t *testing.T) {
	html := `<!doctype html><html><body>
	<h1 class="post-title entry-title">Some Novel Epub</h1>
	<h1 class="post-title entry-title"><a href="https://jnovels.com/some-novel-epub/">Some Novel Epub</a></h1>
	<h1 class="post-title entry-title"><a href="https://jnovels.com/some-novel-epub/">Some Novel Epub</a></h1>
	</body></html>`
	res, err := ParseSearchHTML(html, "Some Novel")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 deduped hit (bare-text h1 skipped), got %d: %+v", len(res), res)
	}
	if res[0].URL != "https://jnovels.com/some-novel-epub/" {
		t.Errorf("url %q", res[0].URL)
	}
}

func TestSearchTitle_emptyQuery(t *testing.T) {
	c := New("t", 3*time.Second)
	res, err := c.SearchTitle("   ")
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Errorf("expected nil for a blank query, got %+v", res)
	}
}

func TestSearchTitle_fetchOverHTTP(t *testing.T) {
	fixture := loadSearchFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("s") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	c := New("test-agent", 5*time.Second)
	// Point the search at the test server by overriding the base URL indirectly:
	// SearchTitle builds jnovelsBaseURL+"/?s=", so exercise the parse path over
	// HTTP via fetch on the test server URL directly.
	doc, err := c.fetch(srv.URL + "/?s=mushoku")
	if err != nil {
		t.Fatal(err)
	}
	res := rankSearchResults(doc, "Mushoku Tensei")
	if len(res) == 0 || res[0].Title == "" {
		t.Fatalf("expected ranked results over HTTP, got %+v", res)
	}

	if _, err := c.fetch(srv.URL + "/?s="); err == nil {
		t.Error("expected an error on the empty-query 404")
	}
}
