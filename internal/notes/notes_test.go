package notes

import (
	"strings"
	"testing"

	"bookwatch/internal/scraper"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		in, want string
		noSpace  bool
	}{
		{"Download Foo Bar Light Novel Epub", "Foo Bar", false},
		{"Some Title Epub", "Some Title", false},
		{"Title: Subtitle", "Title - Subtitle", false},
		{`Bad<>:"/\|?*Name`, "Bad -Name", false}, // ":" -> " -", then bad chars stripped (ports Python)
		{"Spaced Out Epub", "SpacedOut", true},
	}
	for _, c := range cases {
		if got := Sanitize(c.in, c.noSpace); got != c.want {
			t.Errorf("Sanitize(%q,%v) = %q, want %q", c.in, c.noSpace, got, c.want)
		}
	}
}

func TestIsValidURL(t *testing.T) {
	ok := []string{"https://jnovels.com/x", "http://a.b/c"}
	bad := []string{"", "ftp://x", "jnovels.com", "/rel/path"}
	for _, u := range ok {
		if !IsValidURL(u) {
			t.Errorf("IsValidURL(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if IsValidURL(u) {
			t.Errorf("IsValidURL(%q) = true, want false", u)
		}
	}
}

const fixture = `<html><body>
<h1 class="post-title entry-title">Download Test Novel Light Novel Epub</h1>
<div class="featured-media"><img src="https://cdn.example.com/cover.webp"></div>
<div class="synopsis-description">
  <p>First para.<span>junk</span></p>
  <p><span>only junk</span></p>
  <p>Second para.</p>
</div>
<ol><li>VOLUME 1</li><li>Volume 2</li><li>Extras</li></ol>
</body></html>`

func TestParseNovelHTML(t *testing.T) {
	nd, err := scraper.ParseNovelHTML(fixture, scraper.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if nd.Title != "Download Test Novel Light Novel Epub" {
		t.Errorf("title raw = %q", nd.Title) // raw title; Sanitize happens in notes
	}
	if nd.CoverURL != "https://cdn.example.com/cover.webp" {
		t.Errorf("cover = %q", nd.CoverURL)
	}
	if nd.Volumes != 2 {
		t.Errorf("volumes = %d, want 2", nd.Volumes)
	}
	if nd.Description != "First para. Second para." {
		t.Errorf("description = %q", nd.Description)
	}
}

// Layout B: synopsis in nested #editdescription (no synopsis-description).
const fixtureEditDesc = `<html><body>
<h1 class="post-title entry-title">Some Novel Light Novels Epub</h1>
<div class="featured-media"><img src="https://cdn.example.com/c.jpg"></div>
<h4 class="seriesinfo"><span>Description</span></h4>
<div id="editdescription"><div id="editdescription">
  <p>Real synopsis here.</p>
</div></div>
<div><ol><li>VOLUME 01</li><li>VOLUME 02</li><li>VOLUME 03</li></ol></div>
</body></html>`

func TestParseNovelHTML_editDescriptionFallback(t *testing.T) {
	nd, err := scraper.ParseNovelHTML(fixtureEditDesc, scraper.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if nd.Description != "Real synopsis here." {
		t.Errorf("description = %q, want %q", nd.Description, "Real synopsis here.")
	}
	if nd.Volumes != 3 {
		t.Errorf("volumes = %d, want 3", nd.Volumes)
	}
}

func TestBuildNote(t *testing.T) {
	nd := scraper.NovelData{
		Title:       "Download Test Novel Light Novel Epub",
		CoverURL:    "https://cdn.example.com/cover.webp",
		Description: "A synopsis.",
		Volumes:     2,
	}
	out := BuildNote(nd, "https://jnovels.com/test", "cover_TestNovel.webp", "2026-06-17")

	for _, must := range []string{
		"Series: Test Novel",
		"Link: https://jnovels.com/test",
		"Volumes: 2",
		"Read Volumes:",
		`Cover: "[[cover_TestNovel.webp]]"`,
		`- "#LightNovel"`,
		"Status:",
		"Series Status:",
		"created: 2026-06-17",
		"modified: 2026-06-17",
		"### Test Novel",
		"![[cover_TestNovel.webp]]",
		"[[Light Novel]]",
		"A synopsis.",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("BuildNote missing %q:\n%s", must, out)
		}
	}
}

func TestCoverExt(t *testing.T) {
	cases := map[string]string{
		"https://x/y/cover.webp":     ".webp",
		"https://x/y/cover.JPG":      ".jpg",
		"https://x/y/cover":          ".jpg",
		"https://x/y/cover.png?v=2":  ".png",
	}
	for in, want := range cases {
		if got := coverExt(in); got != want {
			t.Errorf("coverExt(%q) = %q, want %q", in, got, want)
		}
	}
}
