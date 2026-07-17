package scraper

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

func loadSearchFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/jnovels_search.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Against saved jnovels search HTML: pdf posts are dropped (epub only), so only
// the epub hits remain — the aggregate "Light Novel" page ranks above the
// single-volume posts. The page-title and widget <h1>s (no child <a>) must be
// skipped, and the surviving titles are cleaned ("Download …"/format words gone).
func TestParseSearchHTML_ranking(t *testing.T) {
	res, err := ParseSearchHTML(loadSearchFixture(t), "Mushoku Tensei: Redundant Reincarnation")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-light-novel-epub/",
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-3-epub/",
		"https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-2-epub/",
	}
	if len(res) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(res), len(want), res)
	}
	for i, w := range want {
		if res[i].URL != w {
			t.Errorf("result[%d] URL = %q, want %q", i, res[i].URL, w)
		}
	}
	if res[0].Title != "Mushoku Tensei: Redundant Reincarnation" {
		t.Errorf("top title %q, want cleaned aggregate title", res[0].Title)
	}
}

// Webnovel-translation posts and pdf twins are excluded from search results, and
// "Download …"/format words are stripped from the display title (#89 feedback).
func TestParseSearchHTML_excludesWebnovelAndPdf(t *testing.T) {
	html := `<!doctype html><html><body>
	<h1 class="post-title entry-title"><a href="https://jnovels.com/kumo-webnovel/">[WEBNOVEL][PDF][EPUB] Kumo Desu ga, Nani ka</a></h1>
	<h1 class="post-title entry-title"><a href="https://jnovels.com/kumo-volume-7-epub/">Download Kumo Desu ga Nani ka Volume 7 Light Novel Epub</a></h1>
	<h1 class="post-title entry-title"><a href="https://jnovels.com/kumo-volume-7-pdf/">Download Kumo Desu ga Nani ka Volume 7 Light Novel Pdf</a></h1>
	</body></html>`
	res, err := ParseSearchHTML(html, "Kumo Desu ga Nani ka")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected only the epub volume post, got %d: %+v", len(res), res)
	}
	if res[0].URL != "https://jnovels.com/kumo-volume-7-epub/" {
		t.Errorf("url %q, want the epub post", res[0].URL)
	}
	if res[0].Title != "Kumo Desu ga Nani ka Volume 7" {
		t.Errorf("title %q, want the cleaned volume title", res[0].Title)
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

// A jnovels single-volume post carries a "Refer to original post" link to the
// aggregate series page; parseOriginalPost must surface it (and return "" for a
// page without one) so the add flow resolves to the series (#89). The parser is
// shared with the Discover resolve path (#91).
func TestOriginalPostLink(t *testing.T) {
	vol, err := goquery.NewDocumentFromReader(strings.NewReader(
		`<!doctype html><html><body><p>Single volume — ` +
			`<a href="https://jnovels.com/kumo-desu-ga-nani-ka-light-novel-epub/">Refer to original post</a></p></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	got := parseOriginalPost(vol)
	want := "https://jnovels.com/kumo-desu-ga-nani-ka-light-novel-epub/"
	if got != want {
		t.Errorf("parseOriginalPost = %q, want %q", got, want)
	}

	noLink, err := goquery.NewDocumentFromReader(strings.NewReader(`<!doctype html><html><body><a href="/x">Home</a></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	if got := parseOriginalPost(noLink); got != "" {
		t.Errorf("expected no link on an aggregate page, got %q", got)
	}
}

// CollapseToSeries reduces per-volume posts of the same series to one entry
// (the best-ranked representative), rewriting the title to the series name and
// keeping distinct series separate (#89).
func TestCollapseToSeries(t *testing.T) {
	in := []SearchResult{
		{Title: "Kumo Desu ga Nani ka Volume 7", URL: "https://jnovels.com/kumo-volume-7-epub/"},
		{Title: "Kumo Desu ga Nani ka Volume 6", URL: "https://jnovels.com/kumo-volume-6-epub/"},
		{Title: "Kumo Desu ga Nani ka Volume 3", URL: "https://jnovels.com/kumo-volume-3-epub/"},
		{Title: "Overlord", URL: "https://jnovels.com/overlord-light-novel-epub/"},
	}
	got := CollapseToSeries(in)
	if len(got) != 2 {
		t.Fatalf("got %d series, want 2: %+v", len(got), got)
	}
	if got[0].Title != "Kumo Desu ga Nani ka" {
		t.Errorf("series title = %q, want %q", got[0].Title, "Kumo Desu ga Nani ka")
	}
	if got[0].URL != "https://jnovels.com/kumo-volume-7-epub/" {
		t.Errorf("representative URL = %q, want the best-ranked (volume 7) post", got[0].URL)
	}
	if got[1].Title != "Overlord" {
		t.Errorf("second series = %q, want Overlord", got[1].Title)
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
