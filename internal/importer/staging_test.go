package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bookwatch/internal/vault"
)

const testToday = "2026-07-08"

// writeCover drops a fake cover file so stageCover has something to copy, and
// returns its path.
func writeCover(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("JPEGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(longPath(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// --- LN series ------------------------------------------------------------

func TestStageLNSeriesMatchedAllDone(t *testing.T) {
	dir := t.TempDir()
	cover := writeCover(t, filepath.Join(dir, "src", "cover.jpg"))
	w := NewWriter(dir, testToday)

	res, err := w.StageLNSeries(PlanLNSeries{
		Series:         "Overlord",
		Author:         "Kugane Maruyama",
		Language:       "eng",
		Link:           "https://jnovels.com/overlord/",
		JnovelsVolumes: 16,
		Description:    "Momonga stays logged in.",
		CoverPath:      cover,
		Volumes: []PlanVolume{
			{Title: "Overlord Vol 1", SeriesIndex: 1, Language: "eng", Description: "v1", Done: true},
			{Title: "Overlord Vol 2", SeriesIndex: 2, Language: "eng", Description: "v2", Done: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := readFile(t, res.Note)
	wants := []string{
		"Series: Overlord",
		"Link: https://jnovels.com/overlord/",
		"Volumes: 16",
		"Read Volumes: 16", // all volumes Done → Read Volumes = Volumes
		`  - Completed`,     // …and Completed
		`  - "#LightNovel"`,
		"Template_used: LightNovelTemplate",
		"Language: eng",
		`Cover: "[[cover_Overlord.jpg]]"`,
		"[[Light Novel]]",
		"Momonga stays logged in.",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("series note missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "#import/unmatched") {
		t.Error("matched series should not carry #import/unmatched")
	}
	if len(res.VolumeNotes) != 2 {
		t.Fatalf("want 2 volume notes, got %d", len(res.VolumeNotes))
	}

	// Volume note is untracked: no Link, no Template_used, tagged #LNVolume.
	vol := readFile(t, res.VolumeNotes[0])
	for _, want := range []string{"Title: Overlord Vol 1", "Series: Overlord", "Series Index: 1", `  - "#LNVolume"`} {
		if !strings.Contains(vol, want) {
			t.Errorf("volume note missing %q\n---\n%s", want, vol)
		}
	}
	for _, bad := range []string{"Link:", "Template_used:"} {
		if strings.Contains(vol, bad) {
			t.Errorf("volume note should not contain %q (would make it tracked)\n%s", bad, vol)
		}
	}
	// Volume note lives under _volumes/<Series>/.
	if !strings.Contains(filepath.ToSlash(res.VolumeNotes[0]), "_volumes/Overlord/") {
		t.Errorf("volume note not under _volumes/Overlord/: %s", res.VolumeNotes[0])
	}
	// Series cover was copied; volumes had none.
	if len(res.Covers) != 1 {
		t.Fatalf("want 1 cover, got %d: %v", len(res.Covers), res.Covers)
	}
	if _, err := os.Stat(res.Covers[0]); err != nil {
		t.Errorf("cover not staged: %v", err)
	}
}

func TestStageLNSeriesPartialNotAllDone(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, testToday)
	res, err := w.StageLNSeries(PlanLNSeries{
		Series: "Mushoku",
		Volumes: []PlanVolume{
			{Title: "Mushoku Vol 1", SeriesIndex: 1, Done: true},
			{Title: "Mushoku Vol 2", SeriesIndex: 2, Done: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, res.Note)
	if !strings.Contains(got, "Read Volumes: \n") {
		t.Errorf("partial series should have blank Read Volumes\n%s", got)
	}
	if !strings.Contains(got, "  - Backlog") {
		t.Errorf("partial series should be Backlog\n%s", got)
	}
	// Volumes falls back to the Calibre count when jnovels count is unknown.
	if !strings.Contains(got, "Volumes: 2") {
		t.Errorf("want Volumes: 2 from Calibre count\n%s", got)
	}
}

func TestStageLNSeriesUnmatched(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, testToday)
	res, err := w.StageLNSeries(PlanLNSeries{
		Series:    "Obscure LN",
		Link:      "https://unmatched.bookwatch.invalid/obscure-ln",
		Unmatched: true,
		Candidates: []Candidate{
			{Title: "Maybe This One", URL: "https://jnovels.com/maybe/"},
		},
		Volumes: []PlanVolume{{Title: "Obscure LN Vol 1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, res.Note)
	for _, want := range []string{
		`  - "#import/unmatched"`,
		"candidate links to review",
		"[Maybe This One](https://jnovels.com/maybe/)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unmatched note missing %q\n%s", want, got)
		}
	}
}

func TestStageLNSeriesMigration(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, testToday)
	res, err := w.StageLNSeries(PlanLNSeries{
		Series:       "Re Zero",
		Link:         "https://jnovels.com/re-zero/",
		ExistingNote: "Re Zero",
		Volumes:      []PlanVolume{{Title: "Re Zero Vol 1", Done: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, res.Note)
	if !strings.Contains(got, `  - "#import/duplicate"`) {
		t.Errorf("migration note should carry #import/duplicate\n%s", got)
	}
	if !strings.Contains(got, "[[Re Zero]]") {
		t.Errorf("migration note should wikilink the existing note\n%s", got)
	}
}

// --- book -----------------------------------------------------------------

func TestStageBookMatchedDone(t *testing.T) {
	dir := t.TempDir()
	cover := writeCover(t, filepath.Join(dir, "src", "cover.jpg"))
	w := NewWriter(dir, testToday)
	res, err := w.StageBook(PlanBook{
		Title:       "The Institute",
		Author:      "Stephen King",
		Language:    "eng",
		Series:      "Standalone",
		SeriesIndex: 3.5,
		Link:        "https://openlibrary.org/works/OL1W",
		WorkID:      "OL1W",
		Done:        true,
		ReleasedEN:  "2019",
		Description: "Kids with powers.",
		CoverPath:   cover,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, res.Note)
	for _, want := range []string{
		"Title: The Institute",
		"Work ID: OL1W",
		"Released EN: 2019",
		"Series: Standalone",
		"Series Index: 3.5",
		"  - Completed",
		`  - "#Book"`,
		"Template_used: BookTemplate",
		"Kids with powers.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("book note missing %q\n%s", want, got)
		}
	}
}

func TestStageBookBacklogAndDup(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, testToday)
	res, err := w.StageBook(PlanBook{
		Title:        "Duplicated",
		Link:         "https://openlibrary.org/works/OL2W",
		ExistingNote: "Duplicated (old)",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, res.Note)
	if !strings.Contains(got, "  - Backlog") {
		t.Errorf("not-done book should be Backlog\n%s", got)
	}
	if !strings.Contains(got, `  - "#import/duplicate"`) || !strings.Contains(got, "[[Duplicated (old)]]") {
		t.Errorf("dup book should tag + wikilink existing\n%s", got)
	}
	if strings.Contains(got, "Series:") {
		t.Errorf("standalone book should omit Series field\n%s", got)
	}
}

// --- series index ---------------------------------------------------------

func TestStageSeriesIndex(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, testToday)
	res, err := w.StageSeriesIndex(PlanSeriesIndex{
		Series:   "Jack Reacher",
		Language: "eng",
		Volumes:  []string{"Killing Floor", "Die Trying"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, res.Note)
	for _, want := range []string{`  - "#import/series"`, "- [[Killing Floor]]", "- [[Die Trying]]"} {
		if !strings.Contains(got, want) {
			t.Errorf("series index missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "Template_used:") || strings.Contains(got, "Link:") {
		t.Errorf("series index must be inert (no Link/Template)\n%s", got)
	}
}

// --- MAX_PATH -------------------------------------------------------------

func TestStageLongPath(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, testToday)
	longSeries := strings.Repeat("A very long light novel series name ", 3) // ~108 chars
	longVol := strings.Repeat("An exceedingly verbose volume subtitle ", 3) // ~117 chars
	res, err := w.StageLNSeries(PlanLNSeries{
		Series:  longSeries,
		Link:    "https://jnovels.com/x/",
		Volumes: []PlanVolume{{Title: longVol + "1", Done: true}},
	})
	if err != nil {
		t.Fatalf("long-path stage failed (MAX_PATH not handled): %v", err)
	}
	if _, err := os.Stat(longPath(res.VolumeNotes[0])); err != nil {
		t.Errorf("long-path volume note not written: %v", err)
	}
}

// --- duplicate index ------------------------------------------------------

func TestDupIndex(t *testing.T) {
	idx := NewDupIndex([]vault.Entry{
		{Title: "Overlord", Link: "https://jnovels.com/overlord/", Kind: "ln"},
		{Title: "The Institute", Link: "https://openlibrary.org/works/OL1W", Kind: "book"},
	})

	if b, ok := idx.Lookup("https://jnovels.com/overlord/", "Whatever"); !ok || b != "Overlord" {
		t.Errorf("link lookup = %q,%v; want Overlord,true", b, ok)
	}
	// Case-insensitive title fallback when the link doesn't match.
	if b, ok := idx.Lookup("https://unmatched.bookwatch.invalid/x", "the  INSTITUTE"); !ok || b != "The Institute" {
		t.Errorf("title lookup = %q,%v; want The Institute,true", b, ok)
	}
	if _, ok := idx.Lookup("https://example.com/nope", "Brand New Book"); ok {
		t.Error("unrelated item should not be a duplicate")
	}
}

func TestFmtIndex(t *testing.T) {
	cases := map[float64]string{0: "", 1: "1", 2.5: "2.5", 10: "10"}
	for in, want := range cases {
		if got := fmtIndex(in); got != want {
			t.Errorf("fmtIndex(%v) = %q, want %q", in, got, want)
		}
	}
}
