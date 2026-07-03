package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bookwatch/internal/scraper"
)

// httptest binds to loopback, which the SSRF guard blocks by default.
func init() { scraper.AllowPrivateHosts = true }

// --- fixtures ---

const searchFixture = `{
  "docs": [
    {
      "key": "/works/OL4966121W",
      "title": "The Warded Man",
      "author_name": ["Peter V. Brett"],
      "author_key": ["OL7891610A"],
      "first_publish_year": 2008,
      "language": ["eng"],
      "cover_i": 7890277
    }
  ]
}`

const authorSearchFixture = `{
  "docs": [
    {
      "key": "OL7891610A",
      "name": "Peter V. Brett",
      "work_count": 12
    }
  ]
}`

const authorWorksFixture = `{
  "docs": [
    {
      "key": "/works/OL4966121W",
      "title": "The Warded Man",
      "first_publish_year": 2008,
      "language": ["eng", "pol"],
      "cover_i": 7890277
    },
    {
      "key": "/works/OL5738618W",
      "title": "The Desert Spear",
      "first_publish_year": 2010,
      "language": [],
      "cover_i": 0
    }
  ]
}`

const workDetailFixture = `{
  "title": "The Warded Man",
  "first_publish_date": "2008"
}`

const editionsFixture = `{
  "entries": [
    {
      "title": "The Warded Man",
      "languages": [{"key": "/languages/eng"}],
      "covers": [7890277]
    },
    {
      "title": "Malowany człowiek",
      "languages": [{"key": "/languages/pol"}],
      "covers": [9876543]
    },
    {
      "languages": [],
      "covers": []
    }
  ]
}`

// newTestServer starts an httptest server and returns a client pointed at it.
func newTestServer(t *testing.T) (*OLClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/search.json":
			if strings.HasPrefix(r.URL.Query().Get("q"), "author_key:") {
				w.Write([]byte(authorWorksFixture))
			} else {
				w.Write([]byte(searchFixture))
			}
		case p == "/search/authors.json":
			w.Write([]byte(authorSearchFixture))
		case strings.HasSuffix(p, "/editions.json"):
			w.Write([]byte(editionsFixture))
		case strings.HasSuffix(p, ".json"):
			w.Write([]byte(workDetailFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	c := NewOpenLibrary("bookwatch-test/1.0", 5*time.Second)
	c.baseURL = srv.URL
	c.coversURL = srv.URL
	return c, srv
}

func TestSearchByTitle(t *testing.T) {
	c, srv := newTestServer(t)
	defer srv.Close()

	got, err := c.SearchByTitle("The Warded Man")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	r := got[0]
	if r.Title != "The Warded Man" {
		t.Errorf("title %q", r.Title)
	}
	if r.Author != "Peter V. Brett" {
		t.Errorf("author %q", r.Author)
	}
	if r.AuthorKey != "OL7891610A" {
		t.Errorf("author key %q", r.AuthorKey)
	}
	if r.Year != 2008 {
		t.Errorf("year %d", r.Year)
	}
	if r.Language != "eng" {
		t.Errorf("language %q", r.Language)
	}
	if r.WorkID != "OL4966121W" {
		t.Errorf("work_id %q", r.WorkID)
	}
	if !strings.Contains(r.CoverURL, "7890277") {
		t.Errorf("cover URL %q should contain cover id", r.CoverURL)
	}
	if !strings.Contains(r.OLURL, "OL4966121W") {
		t.Errorf("ol_url %q should contain work id", r.OLURL)
	}
}

func TestAuthorSearch(t *testing.T) {
	c, srv := newTestServer(t)
	defer srv.Close()

	got, err := c.AuthorSearch("Peter V. Brett")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 result, got %d", len(got))
	}
	a := got[0]
	if a.Name != "Peter V. Brett" {
		t.Errorf("name %q", a.Name)
	}
	if a.OLAuthorID != "OL7891610A" {
		t.Errorf("author id %q", a.OLAuthorID)
	}
	if a.WorkCount != 12 {
		t.Errorf("work_count %d", a.WorkCount)
	}
}

func TestAuthorWorks(t *testing.T) {
	c, srv := newTestServer(t)
	defer srv.Close()

	got, err := c.AuthorWorks("OL7891610A")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 works, got %d", len(got))
	}
	if got[0].WorkID != "OL4966121W" {
		t.Errorf("work[0] id %q", got[0].WorkID)
	}
	if got[0].FirstPubYear != 2008 {
		t.Errorf("work[0] year %d", got[0].FirstPubYear)
	}
	if got[1].FirstPubYear != 2010 {
		t.Errorf("work[1] year %d", got[1].FirstPubYear)
	}
	if got[0].CoverURL == "" {
		t.Error("work[0] cover_i:7890277 should produce a cover URL")
	}
	if got[1].CoverURL != "" {
		t.Error("work[1] cover_i:0 should produce empty CoverURL")
	}
	if got[0].Language != "eng" {
		t.Errorf("work[0] language %q", got[0].Language)
	}
	if got[1].Language != "" {
		t.Errorf("work[1] language should be empty, got %q", got[1].Language)
	}
	if want := []string{"eng", "pol"}; !slicesEqual(got[0].Languages, want) {
		t.Errorf("work[0] languages %v, want %v", got[0].Languages, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWorkDetail(t *testing.T) {
	c, srv := newTestServer(t)
	defer srv.Close()

	got, err := c.WorkDetail("OL4966121W")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkID != "OL4966121W" {
		t.Errorf("work_id %q", got.WorkID)
	}
	if got.Title != "The Warded Man" {
		t.Errorf("title %q", got.Title)
	}
	if got.FirstPubYear != 2008 {
		t.Errorf("year %d", got.FirstPubYear)
	}
	if len(got.Editions) != 3 {
		t.Fatalf("want 3 editions, got %d", len(got.Editions))
	}
	if got.Editions[0].Language != "eng" {
		t.Errorf("ed[0] lang %q", got.Editions[0].Language)
	}
	if got.Editions[0].Title != "The Warded Man" {
		t.Errorf("ed[0] title %q", got.Editions[0].Title)
	}
	if !strings.Contains(got.Editions[0].CoverURL, "7890277") {
		t.Errorf("ed[0] cover %q", got.Editions[0].CoverURL)
	}
	if got.Editions[1].Language != "pol" {
		t.Errorf("ed[1] lang %q", got.Editions[1].Language)
	}
	if got.Editions[1].Title != "Malowany człowiek" {
		t.Errorf("ed[1] title %q", got.Editions[1].Title)
	}
	if !strings.Contains(got.Editions[1].CoverURL, "9876543") {
		t.Errorf("ed[1] cover %q", got.Editions[1].CoverURL)
	}
	// third edition has no language or cover
	if got.Editions[2].CoverURL != "" {
		t.Errorf("ed[2] cover should be empty, got %q", got.Editions[2].CoverURL)
	}
}

func TestSelectCover(t *testing.T) {
	w := Work{
		Editions: []Edition{
			{Language: "eng", CoverURL: "eng.jpg"},
			{Language: "pol", CoverURL: "pol.jpg"},
		},
	}
	if got := SelectCover(w, "pol"); got != "pol.jpg" {
		t.Errorf("want pol.jpg, got %q", got)
	}
	// falls back to first cover when lang not found
	if got := SelectCover(w, "deu"); got != "eng.jpg" {
		t.Errorf("want eng.jpg fallback, got %q", got)
	}
	// no editions -> empty
	if got := SelectCover(Work{}, "eng"); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestFindEdition(t *testing.T) {
	eds := []Edition{
		{Language: "eng", Title: "Season of Storms", CoverURL: "eng.jpg"},
		{Language: "pol", Title: "Sezon Burz", CoverURL: "pol.jpg"},
	}
	ed, ok := FindEdition(eds, "pol")
	if !ok || ed.Title != "Sezon Burz" {
		t.Errorf("want the Polish edition, got %+v ok=%v", ed, ok)
	}
	if _, ok := FindEdition(eds, "ger"); ok {
		t.Error("no German edition exists, want ok=false")
	}
	if _, ok := FindEdition(nil, "eng"); ok {
		t.Error("empty editions list, want ok=false")
	}
}

func TestParseYear(t *testing.T) {
	cases := []struct{ in string; want int }{
		{"2008", 2008},
		{"April 2010", 2010},
		{"", 0},
		{"not a year", 0},
	}
	for _, tc := range cases {
		if got := parseYear(tc.in); got != tc.want {
			t.Errorf("parseYear(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

const workFullFixture = `{
  "title": "The Warded Man",
  "first_publish_date": "2008",
  "covers": [7890277],
  "authors": [{"author": {"key": "/authors/OL7891610A"}}]
}`

const authorDetailFixture = `{"name": "Peter V. Brett"}`

func TestWorkByID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/works/OL4966121W.json":
			w.Write([]byte(workFullFixture))
		case r.URL.Path == "/authors/OL7891610A.json":
			w.Write([]byte(authorDetailFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewOpenLibrary("t", 5*time.Second)
	c.baseURL = srv.URL
	c.coversURL = srv.URL

	got, err := c.WorkByID("OL4966121W")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "The Warded Man" {
		t.Errorf("title %q", got.Title)
	}
	if got.Author != "Peter V. Brett" {
		t.Errorf("author %q", got.Author)
	}
	if got.AuthorKey != "OL7891610A" {
		t.Errorf("author key %q", got.AuthorKey)
	}
	if got.Year != 2008 {
		t.Errorf("year %d", got.Year)
	}
	if got.WorkID != "OL4966121W" {
		t.Errorf("work id %q", got.WorkID)
	}
	if !strings.Contains(got.CoverURL, "7890277") {
		t.Errorf("cover %q", got.CoverURL)
	}
	if !strings.Contains(got.OLURL, "OL4966121W") {
		t.Errorf("ol url %q", got.OLURL)
	}
}

func TestNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := NewOpenLibrary("t", 3*time.Second)
	c.baseURL = srv.URL
	c.coversURL = srv.URL

	if _, err := c.SearchByTitle("x"); err == nil {
		t.Error("expected error on 404")
	}
}

func TestRateLimitRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewOpenLibrary("t", 3*time.Second)
	c.baseURL = srv.URL
	c.coversURL = srv.URL
	c.retryDelay = 0

	_, err := c.SearchByTitle("x")
	if err == nil {
		t.Error("expected rate-limit error")
	}
	if attempts != 3 {
		t.Errorf("want 3 attempts, got %d", attempts)
	}
}
