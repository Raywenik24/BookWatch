package notes

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bookwatch/internal/scraper"
)

func init() { scraper.AllowPrivateHosts = true } // httptest binds to loopback

func TestCoverName(t *testing.T) {
	if got := CoverName("Rich Dad: Poor Dad", ".png"); got != "cover_RichDad-PoorDad.png" {
		t.Errorf("CoverName: %q", got)
	}
}

func TestSanitize_allVolumes(t *testing.T) {
	cases := map[string]string{
		"Download Kumo Desu ga Nani ka all volumes Epub": "Kumo Desu ga Nani ka", // full jnovels aggregate title
		"Kumo Desu ga Nani ka all volumes":               "Kumo Desu ga Nani ka",
		"Kumo Desu ga Nani ka ALL VOLUMES":               "Kumo Desu ga Nani ka", // case-insensitive
		"Overlord":                                       "Overlord",             // no-op
		"The All Volumes Society":                        "The All Volumes Society", // only a trailing tag is stripped
	}
	for in, want := range cases {
		if got := Sanitize(in, false); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeBookNote(t *testing.T, dir, base string) string {
	t.Helper()
	p := filepath.Join(dir, base+".md")
	content := BuildBookNote(base, "Andrzej Sapkowski",
		"https://openlibrary.org/works/OL123W", "OL123W", "cover_"+base+".jpg",
		"Backlog", "1993", "A witcher tale.", "2026-07-05")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// RenameNote must move the .md, rename the cover attachment to follow, and fix
// the in-body heading + Cover embed — including the Polish-diacritic case where
// the on-disk name gained characters the search source had stripped.
func TestRenameNote_polishAndCoverFollow(t *testing.T) {
	vaultDir := t.TempDir()
	attach := filepath.Join(vaultDir, "att")
	if err := os.MkdirAll(attach, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTitle := "Wiedzmin Ostatnie zyczenie"
	oldPath := writeBookNote(t, vaultDir, oldTitle)
	oldCover := CoverName(oldTitle, ".jpg")
	if err := os.WriteFile(filepath.Join(attach, oldCover), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}

	o := Options{VaultDir: vaultDir, AttachmentsDir: "att"}
	newTitle := "Wiedźmin - Ostatnie Życzenie"
	newPath, newCover, err := RenameNote(o, oldPath, oldCover, newTitle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old note still present: %v", err)
	}
	if filepath.Base(newPath) != Sanitize(newTitle, false)+".md" {
		t.Errorf("new note name: %q", newPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new note missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(attach, newCover)); err != nil {
		t.Errorf("renamed cover missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(attach, oldCover)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old cover still present")
	}
	body, _ := os.ReadFile(newPath)
	if !strings.Contains(string(body), "### "+Sanitize(newTitle, false)) {
		t.Errorf("heading not updated:\n%s", body)
	}
	if !strings.Contains(string(body), "Title: "+Sanitize(newTitle, false)) {
		t.Errorf("Title frontmatter field not updated:\n%s", body)
	}
	if strings.Contains(string(body), "Title: "+oldTitle) {
		t.Errorf("old Title frontmatter still present:\n%s", body)
	}
	if !strings.Contains(string(body), "![["+newCover+"]]") {
		t.Errorf("cover embed not updated:\n%s", body)
	}
}

// A rename onto an existing sibling note must be refused, not clobber it.
func TestRenameNote_collision(t *testing.T) {
	vaultDir := t.TempDir()
	oldPath := writeBookNote(t, vaultDir, "Source Book")
	writeBookNote(t, vaultDir, "Target Book")
	o := Options{VaultDir: vaultDir, AttachmentsDir: "att"}
	if _, _, err := RenameNote(o, oldPath, "", "Target Book"); !errors.Is(err, ErrNoteExists) {
		t.Errorf("expected ErrNoteExists, got %v", err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("source note should be untouched: %v", err)
	}
}

// Create scrapes + writes a note; a second add whose title sanitizes to the
// same filename must be refused (ErrNoteExists) and leave the original intact.
func TestCreate_atomicAndRefusesOverwrite(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cover.webp" {
			w.Header().Set("Content-Type", "image/webp")
			w.Write([]byte("fake-image-bytes"))
			return
		}
		fmt.Fprintf(w, `<html><body>
<h1 class="post-title entry-title">Dupe Title Epub</h1>
<div class="featured-media"><img src="%s/cover.webp"></div>
<div class="synopsis-description"><p>Desc.</p></div>
<ol><li>VOLUME 1</li></ol>
</body></html>`, srv.URL)
	}))
	defer srv.Close()

	o := Options{VaultDir: t.TempDir(), NewNoteDir: "LN", AttachmentsDir: "LN/_att"}
	sc := scraper.New("test", 5*time.Second)

	res, err := Create(o, sc, nil, scraper.DefaultRules(), srv.URL, "")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	before, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("note not written: %v", err)
	}

	// Different link, same sanitized title → same path → must refuse.
	_, err = Create(o, sc, nil, scraper.DefaultRules(), srv.URL+"/other", "")
	if !errors.Is(err, ErrNoteExists) {
		t.Fatalf("expected ErrNoteExists, got %v", err)
	}
	after, _ := os.ReadFile(res.Path)
	if string(before) != string(after) {
		t.Error("existing note was modified by the refused create")
	}
}

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

func TestBuildNote_hasAuthorField(t *testing.T) {
	nd := scraper.NovelData{Title: "My Novel Epub", Volumes: 1}
	out := BuildNote(nd, "https://jnovels.com/x", "c.jpg", "", "2026-06-29")
	if !strings.Contains(out, "Author:") {
		t.Errorf("BuildNote missing Author field:\n%s", out)
	}
}

func TestBuildNote_status(t *testing.T) {
	nd := scraper.NovelData{Title: "My Novel", Volumes: 1}
	// Explicit status is used.
	if out := BuildNote(nd, "https://jnovels.com/x", "c.jpg", "Completed", "2026-06-29"); !strings.Contains(out, "\n  - Completed\n") {
		t.Errorf("BuildNote missing chosen status Completed:\n%s", out)
	}
	// Empty falls back to Backlog (the historical default).
	if out := BuildNote(nd, "https://jnovels.com/x", "c.jpg", "", "2026-06-29"); !strings.Contains(out, "\n  - Backlog\n") {
		t.Errorf("BuildNote missing default status Backlog:\n%s", out)
	}
}

func TestBuildBookNote(t *testing.T) {
	out := BuildBookNote("Rich Dad Poor Dad", "Robert T. Kiyosaki",
		"https://openlibrary.org/works/OL20749838W", "OL20749838W",
		"cover_RichDadPoorDad.jpg", "Completed", "1997", "A synopsis.", "2026-06-29")

	for _, must := range []string{
		"Title: Rich Dad Poor Dad",
		"Author: Robert T. Kiyosaki",
		"Link: https://openlibrary.org/works/OL20749838W",
		"Work ID: OL20749838W",
		`Cover: "[[cover_RichDadPoorDad.jpg]]"`,
		"Released EN: 1997",
		"  - Completed",
		`- "#Book"`,
		"Template_used: BookTemplate",
		"created: 2026-06-29",
		"### Rich Dad Poor Dad",
		"![[cover_RichDadPoorDad.jpg]]",
		"A synopsis.",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("BuildBookNote missing %q:\n%s", must, out)
		}
	}
}

func TestBuildBookNote_emptyCover(t *testing.T) {
	out := BuildBookNote("Some Book", "Author", "https://openlibrary.org/works/OLxW", "OLxW", "", "Backlog", "2020", "", "2026-06-29")
	if strings.Contains(out, `"[["`) {
		t.Errorf("empty cover should not produce [[...]] notation:\n%s", out)
	}
	if strings.Contains(out, "![[") {
		t.Errorf("empty cover should not produce an embed:\n%s", out)
	}
}

func TestBuildBookNote_defaultStatus(t *testing.T) {
	out := BuildBookNote("Some Book", "Author", "https://openlibrary.org/works/OLxW", "OLxW", "", "", "2020", "", "2026-06-29")
	if !strings.Contains(out, "  - Backlog") {
		t.Errorf("empty status should default to Backlog:\n%s", out)
	}
}

// CreateBook writes a #Book note from catalog data (no scraping) and refuses
// a duplicate filename, mirroring Create's duplicate-filename guard.
func TestCreateBook(t *testing.T) {
	o := Options{VaultDir: t.TempDir(), NewNoteDir: "Books", AttachmentsDir: "Books/_att"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("fake-cover-bytes"))
	}))
	defer srv.Close()

	res, err := CreateBook(o, nil, "Rich Dad Poor Dad", "Robert T. Kiyosaki",
		"https://openlibrary.org/works/OL20749838W", "OL20749838W", srv.URL+"/cover.jpg", "Completed", "1997", "A synopsis.")
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	if res.Cover == "" {
		t.Error("expected a cover filename")
	}
	raw, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("note not written: %v", err)
	}
	if !strings.Contains(string(raw), "  - Completed") {
		t.Errorf("note should carry the chosen status:\n%s", raw)
	}
	if !strings.Contains(string(raw), "![[cover_RichDadPoorDad.jpg]]") {
		t.Errorf("note should embed the cover:\n%s", raw)
	}
	if !strings.Contains(string(raw), "A synopsis.") {
		t.Errorf("note should carry the description:\n%s", raw)
	}
	if !strings.Contains(string(raw), "Released EN: 1997") {
		t.Errorf("note should carry the released year:\n%s", raw)
	}

	if _, err := CreateBook(o, nil, "Rich Dad Poor Dad", "Robert T. Kiyosaki",
		"https://openlibrary.org/works/OLother", "OLother", "", "Backlog", "", ""); !errors.Is(err, ErrNoteExists) {
		t.Fatalf("expected ErrNoteExists for same-title note, got %v", err)
	}
}

func TestBuildNote(t *testing.T) {
	nd := scraper.NovelData{
		Title:       "Download Test Novel Light Novel Epub",
		CoverURL:    "https://cdn.example.com/cover.webp",
		Description: "A synopsis.",
		Volumes:     2,
	}
	out := BuildNote(nd, "https://jnovels.com/test", "cover_TestNovel.webp", "", "2026-06-17")

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
		"https://x/y/cover.webp":    ".webp",
		"https://x/y/cover.JPG":     ".jpg",
		"https://x/y/cover":         ".jpg",
		"https://x/y/cover.png?v=2": ".png",
	}
	for in, want := range cases {
		if got := coverExt(in); got != want {
			t.Errorf("coverExt(%q) = %q, want %q", in, got, want)
		}
	}
}
