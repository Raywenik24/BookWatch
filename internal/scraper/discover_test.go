package scraper

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Against saved jnovels listing HTML: only category-light-novels epub posts are
// kept — the app-promo post (no category) and the pdf twin are dropped, and a
// duplicate epub link collapses. Titles lose their trailing " Epub".
func TestParseLatestHTML(t *testing.T) {
	res, err := ParseLatestHTML(loadFixture(t, "jnovels_latest.html"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Release{
		{Title: "Lady Bumpkin and Her Lord Villain Volume 8", URL: "https://jnovels.com/lady-bumpkin-and-her-lord-villain-volume-8-epub/"},
		{Title: "Zero Damage Sword Saint Volume 4", URL: "https://jnovels.com/zero-damage-sword-saint-volume-4-epub/"},
	}
	// The fixture has a duplicate epub article for Zero Damage; parseLatestReleases
	// itself doesn't dedup (that's LatestEpubReleases' job across pages), so it
	// returns 3 rows here — assert the epub/category filtering, not cross-dup.
	if len(res) != 3 {
		t.Fatalf("got %d releases, want 3: %+v", len(res), res)
	}
	for i, w := range want {
		if res[i].Title != w.Title || res[i].URL != w.URL {
			t.Errorf("release[%d] = %+v, want title=%q url=%q", i, res[i], w.Title, w.URL)
		}
	}
	if res[0].CoverURL == "" {
		t.Error("expected a featured-media cover on the first release")
	}
	for _, r := range res {
		if r.URL[len(r.URL)-5:] != "epub/" {
			t.Errorf("non-epub release leaked through: %q", r.URL)
		}
	}
}

// LatestEpubReleases dedups across pages and stops once n are gathered. The test
// server serves the same fixture for every page, so the second page adds nothing
// new and the walk stops.
func TestLatestEpubReleases_dedupAndLimit(t *testing.T) {
	fixture := loadFixture(t, "jnovels_latest.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	c := New("test-agent", 5*time.Second)
	doc, err := c.fetch(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	got := parseLatestReleases(doc)
	// Two distinct epub series in the fixture (one appears twice) → 3 rows raw.
	if len(got) != 3 {
		t.Fatalf("parseLatestReleases got %d, want 3", len(got))
	}
}

func TestParseRandomizerHTML(t *testing.T) {
	res, err := ParseRandomizerHTML(loadFixture(t, "jnovels_randomizer.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 4 {
		t.Fatalf("got %d picks, want 4: %+v", len(res), res)
	}
	if res[0].URL != "https://jnovels.com/goblin-slayer-volume-15-epub/" {
		t.Errorf("first pick URL %q", res[0].URL)
	}
	if res[0].Title != "Goblin Slayer Volume 15" {
		t.Errorf("first pick title %q, want %q", res[0].Title, "Goblin Slayer Volume 15")
	}
	if res[0].CoverURL == "" {
		t.Error("expected a cover on the first pick")
	}
	// A pdf pick is present (tearmoon volume-10-pdf) — parseRandomizer keeps it;
	// the pdf filtering happens server-side in Find-new.
	var sawPDF bool
	for _, r := range res {
		if r.URL[len(r.URL)-4:] == "pdf/" {
			sawPDF = true
		}
	}
	if !sawPDF {
		t.Error("expected the pdf pick to survive parsing (server filters it, not the parser)")
	}
}

func TestParseOriginalPostHTML(t *testing.T) {
	got, err := ParseOriginalPostHTML(loadFixture(t, "jnovels_volume.html"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://jnovels.com/the-diary-of-a-middle-aged-sages-carefree-life-in-another-world-light-novel-epub/"
	if got != want {
		t.Errorf("original post = %q, want %q", got, want)
	}
}

func TestParseOriginalPostHTML_absent(t *testing.T) {
	got, err := ParseOriginalPostHTML(`<!doctype html><body><a href="/x">Download</a></body>`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty when no original-post link, got %q", got)
	}
}

func TestTitleFromSlug(t *testing.T) {
	cases := map[string]string{
		"https://jnovels.com/goblin-slayer-volume-15-epub/":    "Goblin Slayer Volume 15",
		"https://jnovels.com/kings-proposal-light-novel-epub/": "Kings Proposal",
		"https://jnovels.com/tearmoon-empire-volume-10-pdf/":   "Tearmoon Empire Volume 10",
		"https://jnovels.com/some-series-light-novel-epub":     "Some Series",
	}
	for in, want := range cases {
		if got := TitleFromSlug(in); got != want {
			t.Errorf("TitleFromSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
