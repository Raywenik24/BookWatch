// Staging model + note writer for the Calibre import (milestone 1.2.0, #74).
//
// Part 4 of 7. Given the fully-resolved import decisions from the matcher (#73)
// and — for light novels — a jnovels scrape (done by the orchestration in #75),
// this turns each Calibre item into staged `.md` notes plus copied covers under a
// staging folder that lives *outside* the scan roots. Review happens in Obsidian;
// nothing here writes into the real Book/LN folders (that's finalize, #76).
//
// The types below are the plan the orchestration hands us — pure data, no network
// and no Calibre reads — so the whole writer is exercised against a temp dir in
// tests. Four note kinds are produced:
//
//   - LN series note   — tracked #LightNovel; real jnovels Link + Volumes when
//     matched, else a synthetic link + #import/unmatched + candidate links.
//   - LN volume note   — one per Calibre volume, untracked (#LNVolume, no
//     Link/Template) so vault.parse ignores it; carries Series: for Dataview.
//   - Regular book note — tracked #Book; real Link/Work ID when matched, else
//     synthetic + #import/unmatched + candidates.
//   - Series index note — inert #import/series listing its volumes as wikilinks.
//
// Duplicates against existing tracked notes are detected with DupIndex; a dup is
// staged anyway, tagged #import/duplicate, with a [[wikilink]] back to the real
// note so the reviewer can hand-merge (an existing LN series is *never* modified —
// only a proposed updated note is staged).
package importer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"bookwatch/internal/notes"
	"bookwatch/internal/vault"
)

// volumesSubdir is where untracked LN volume notes are staged, mirroring the
// vault's real `01_LightNovel_db/_volumes/<Series>/` layout so finalize (#76) can
// move a whole series in one step.
const volumesSubdir = "_volumes"

// PlanVolume is one owned Calibre volume of a light-novel series, staged as an
// untracked archive note. Content is Calibre's; it is never catalog-matched.
type PlanVolume struct {
	Title       string
	SeriesIndex float64
	Language    string
	Released    string // Released EN (catalog date, pubdate fallback) — may be ""
	Description string
	CoverPath   string // absolute path to a local cover.jpg to copy, "" when none
	Done        bool   // carries the Calibre "Done" tag (drives the series' AllDone)
}

// PlanLNSeries is one light-novel series to stage: a tracked #LightNovel series
// note plus an untracked note per owned volume. The match fields come from
// Matcher.MatchSeries; the jnovels* fields from a scrape of the resolved link
// (both done by the orchestration, #75).
type PlanLNSeries struct {
	Series   string // Calibre series name — the note filename and Series: field
	Author   string // primary author(s), joined; may be ""
	Language string // e.g. "eng", "pol"

	Link       string      // resolved jnovels link, or a synthetic https:// link
	Unmatched  bool        // true → synthetic link + #import/unmatched + candidates
	Candidates []Candidate // fallback links dropped into the body when unmatched

	JnovelsVolumes     int    // scraped volume count; 0 when unknown/unmatched
	JnovelsDescription string // description fallback when Calibre's is blank
	Description        string // preferred (Calibre) series description; may be ""
	CoverPath          string // local cover.jpg for the series note, "" when none

	// ExistingNote is the basename (no extension) of an existing tracked
	// #LightNovel note this series duplicates. Non-empty → the staged series note
	// is a *proposal* tagged #import/duplicate with a [[wikilink]] to the real one;
	// the real note is never touched.
	ExistingNote string

	Volumes []PlanVolume
}

// PlanBook is one regular (non-LN) book to stage as a tracked #Book note.
type PlanBook struct {
	Title       string
	Author      string
	Language    string
	Series      string  // "" when standalone
	SeriesIndex float64 // meaningful only when Series != ""

	Link       string
	WorkID     string
	Unmatched  bool
	Candidates []Candidate

	Done        bool // Calibre "Done" tag → Completed, else Backlog
	ReleasedEN  string
	Description string
	CoverPath   string

	ExistingNote string // dup target basename, "" when new
}

// PlanSeriesIndex is an inert index note for a non-LN series: it just lists the
// series' book notes as wikilinks so the reviewer can keep or delete it.
type PlanSeriesIndex struct {
	Series   string
	Language string
	Volumes  []string // titles of the book notes to wikilink
}

// StageResult reports what a stage call wrote, so the orchestration can record it
// (for Start-over cleanup) and the manifest can list it.
type StageResult struct {
	Note        string   // path to the primary (series/book/index) note
	VolumeNotes []string // paths to any staged volume notes
	Covers      []string // paths to any copied cover images
}

// Writer stages notes + covers under a single root directory. today is stamped
// into created/modified; it is a field so tests get a deterministic date.
type Writer struct {
	dir   string
	today string
}

// NewWriter returns a Writer that stages under stagingDir (created on first
// write). today is the YYYY-MM-DD stamp for created/modified.
func NewWriter(stagingDir, today string) *Writer {
	return &Writer{dir: stagingDir, today: today}
}

// StageLNSeries writes the tracked series note plus one untracked note per owned
// volume, copying covers alongside each. Read Volumes / Status follow the rule:
// Completed with Read Volumes = Volumes only when *every* owned volume is Done,
// otherwise Backlog (so the app's status auto-correction can't revert it).
func (w *Writer) StageLNSeries(p PlanLNSeries) (StageResult, error) {
	var res StageResult

	volumes := p.JnovelsVolumes
	if volumes <= 0 {
		volumes = len(p.Volumes)
	}
	allDone := len(p.Volumes) > 0
	for _, v := range p.Volumes {
		if !v.Done {
			allDone = false
			break
		}
	}

	tags := []string{"#LightNovel"}
	if p.Unmatched {
		tags = append(tags, "#import/unmatched")
	}
	if p.ExistingNote != "" {
		tags = append(tags, "#import/duplicate")
	}

	base := notes.Sanitize(p.Series, false)
	notePath := filepath.Join(w.dir, base+".md")

	coverName, coverPath, err := w.stageCover(filepath.Dir(notePath), p.Series, p.CoverPath)
	if err != nil {
		return res, err
	}
	if coverPath != "" {
		res.Covers = append(res.Covers, coverPath)
	}

	desc := firstNonBlank(p.Description, p.JnovelsDescription)
	content := w.buildLNSeriesNote(p, base, coverName, volumes, allDone, desc, tags)
	if err := w.writeNote(notePath, content); err != nil {
		return res, err
	}
	res.Note = notePath

	// Untracked volume notes under _volumes/<Series>/.
	volDir := filepath.Join(w.dir, volumesSubdir, base)
	for _, v := range p.Volumes {
		vp, cover, err := w.stageVolume(volDir, p.Series, v)
		if err != nil {
			return res, err
		}
		res.VolumeNotes = append(res.VolumeNotes, vp)
		if cover != "" {
			res.Covers = append(res.Covers, cover)
		}
	}
	return res, nil
}

// StageBook writes one tracked #Book note (+ cover). Done → Completed, else
// Backlog; an unmatched match adds #import/unmatched + candidate links, a dup adds
// #import/duplicate + a wikilink to the existing note.
func (w *Writer) StageBook(p PlanBook) (StageResult, error) {
	var res StageResult

	tags := []string{"#Book"}
	if p.Unmatched {
		tags = append(tags, "#import/unmatched")
	}
	if p.ExistingNote != "" {
		tags = append(tags, "#import/duplicate")
	}
	status := "Backlog"
	if p.Done {
		status = "Completed"
	}

	base := notes.Sanitize(p.Title, false)
	notePath := filepath.Join(w.dir, base+".md")

	coverName, coverPath, err := w.stageCover(filepath.Dir(notePath), p.Title, p.CoverPath)
	if err != nil {
		return res, err
	}
	if coverPath != "" {
		res.Covers = append(res.Covers, coverPath)
	}

	content := w.buildBookNote(p, base, coverName, status, tags)
	if err := w.writeNote(notePath, content); err != nil {
		return res, err
	}
	res.Note = notePath
	return res, nil
}

// StageSeriesIndex writes an inert #import/series note listing the series' book
// notes as wikilinks. It is not tracked (no Link/Template) — keep-or-delete in
// review.
func (w *Writer) StageSeriesIndex(p PlanSeriesIndex) (StageResult, error) {
	var res StageResult
	base := notes.Sanitize(p.Series, false)
	notePath := filepath.Join(w.dir, base+".md")
	if err := w.writeNote(notePath, w.buildSeriesIndex(p, base)); err != nil {
		return res, err
	}
	res.Note = notePath
	return res, nil
}

// stageVolume writes one untracked volume note into volDir and copies its cover.
func (w *Writer) stageVolume(volDir, series string, v PlanVolume) (notePath, coverPath string, err error) {
	base := notes.Sanitize(v.Title, false)
	notePath = filepath.Join(volDir, base+".md")
	coverName, coverPath, err := w.stageCover(volDir, v.Title, v.CoverPath)
	if err != nil {
		return "", "", err
	}
	if err := w.writeNote(notePath, w.buildLNVolumeNote(series, v, base, coverName)); err != nil {
		return "", "", err
	}
	return notePath, coverPath, nil
}

// stageCover copies a local cover into dir (named cover_<Title>.<ext>, matching
// notes.CoverName so Obsidian resolves the ![[embed]] by filename). It returns
// the wikilink-friendly filename and the written path; both empty when there is
// no source cover.
func (w *Writer) stageCover(dir, title, src string) (name, path string, err error) {
	if strings.TrimSpace(src) == "" {
		return "", "", nil
	}
	ext := filepath.Ext(src)
	if ext == "" {
		ext = ".jpg"
	}
	name = notes.CoverName(title, ext)
	path = filepath.Join(dir, name)
	if err := copyFile(src, path); err != nil {
		return "", "", fmt.Errorf("stage cover %s: %w", title, err)
	}
	return name, path, nil
}

// --- note builders ---------------------------------------------------------

func (w *Writer) buildLNSeriesNote(p PlanLNSeries, title, cover string, volumes int, allDone bool, desc string, tags []string) string {
	status, readVol := "Backlog", ""
	if allDone {
		status, readVol = "Completed", strconv.Itoa(volumes)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Series: %s\n", title)
	fmt.Fprintf(&b, "Author: %s\n", p.Author)
	fmt.Fprintf(&b, "Link: %s\n", p.Link)
	fmt.Fprintf(&b, "Volumes: %d\n", volumes)
	fmt.Fprintf(&b, "Read Volumes: %s\n", readVol)
	writeCoverField(&b, cover)
	fmt.Fprintf(&b, "Language: %s\n", p.Language)
	writeTags(&b, tags)
	b.WriteString("Status:\n")
	fmt.Fprintf(&b, "  - %s\n", status)
	b.WriteString("Template_used: LightNovelTemplate\n")
	b.WriteString("Series Status:\n")
	fmt.Fprintf(&b, "created: %s\n", w.today)
	fmt.Fprintf(&b, "modified: %s\n", w.today)
	b.WriteString("---\n")

	writeBody(&b, title, cover, true, desc, p.ExistingNote, boolCandidates(p.Unmatched, p.Candidates))
	return b.String()
}

func (w *Writer) buildLNVolumeNote(series string, v PlanVolume, title, cover string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Title: %s\n", title)
	fmt.Fprintf(&b, "Series: %s\n", series)
	fmt.Fprintf(&b, "Series Index: %s\n", fmtIndex(v.SeriesIndex))
	fmt.Fprintf(&b, "Language: %s\n", v.Language)
	if v.Released != "" {
		fmt.Fprintf(&b, "Released EN: %s\n", v.Released)
	}
	writeCoverField(&b, cover)
	writeTags(&b, []string{"#LNVolume"})
	fmt.Fprintf(&b, "created: %s\n", w.today)
	fmt.Fprintf(&b, "modified: %s\n", w.today)
	b.WriteString("---\n")

	writeBody(&b, title, cover, false, v.Description, "", nil)
	return b.String()
}

func (w *Writer) buildBookNote(p PlanBook, title, cover, status string, tags []string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Title: %s\n", title)
	fmt.Fprintf(&b, "Author: %s\n", p.Author)
	fmt.Fprintf(&b, "Link: %s\n", p.Link)
	fmt.Fprintf(&b, "Work ID: %s\n", p.WorkID)
	writeCoverField(&b, cover)
	fmt.Fprintf(&b, "Released EN: %s\n", p.ReleasedEN)
	if p.Series != "" {
		fmt.Fprintf(&b, "Series: %s\n", p.Series)
		fmt.Fprintf(&b, "Series Index: %s\n", fmtIndex(p.SeriesIndex))
	}
	fmt.Fprintf(&b, "Language: %s\n", p.Language)
	b.WriteString("Status:\n")
	fmt.Fprintf(&b, "  - %s\n", status)
	writeTags(&b, tags)
	b.WriteString("Template_used: BookTemplate\n")
	fmt.Fprintf(&b, "created: %s\n", w.today)
	fmt.Fprintf(&b, "modified: %s\n", w.today)
	b.WriteString("---\n")

	writeBody(&b, title, cover, false, p.Description, p.ExistingNote, boolCandidates(p.Unmatched, p.Candidates))
	return b.String()
}

func (w *Writer) buildSeriesIndex(p PlanSeriesIndex, title string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Series: %s\n", title)
	fmt.Fprintf(&b, "Language: %s\n", p.Language)
	writeTags(&b, []string{"#import/series"})
	fmt.Fprintf(&b, "created: %s\n", w.today)
	fmt.Fprintf(&b, "modified: %s\n", w.today)
	b.WriteString("---\n")
	fmt.Fprintf(&b, "### %s\n\nVolumes in this series:\n\n", title)
	for _, v := range p.Volumes {
		fmt.Fprintf(&b, "- [[%s]]\n", notes.Sanitize(v, false))
	}
	return b.String()
}

// writeBody renders the shared note body: the H3 title, the cover embed, the LN
// "[[Light Novel]]" link (LN notes only), the description, and — for
// unmatched/duplicate notes — the review aids (a dup wikilink callout, a
// candidate-links list).
func writeBody(b *strings.Builder, title, cover string, isLN bool, desc, dupOf string, candidates []Candidate) {
	fmt.Fprintf(b, "### %s\n\n", title)
	if cover != "" {
		fmt.Fprintf(b, "![[%s]]\n\n", cover)
	}
	if isLN {
		b.WriteString("[[Light Novel]]\n\n")
	}
	if dupOf != "" {
		fmt.Fprintf(b, "> [!warning] Possible duplicate of [[%s]] — review and hand-merge before finalizing.\n\n", dupOf)
	}
	if s := strings.TrimSpace(desc); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}
	if len(candidates) > 0 {
		b.WriteString("\n**Unmatched — candidate links to review:**\n\n")
		for _, c := range candidates {
			fmt.Fprintf(b, "- [%s](%s)\n", c.Title, c.URL)
		}
	}
}

// writeCoverField writes the `Cover: "[[name]]"` frontmatter line, blank when the
// note has no cover.
func writeCoverField(b *strings.Builder, cover string) {
	if cover == "" {
		b.WriteString("Cover:\n")
		return
	}
	fmt.Fprintf(b, "Cover: \"[[%s]]\"\n", cover)
}

// writeTags writes a `tags:` block list, each entry as `  - "#Tag"` to match the
// other builders.
func writeTags(b *strings.Builder, tags []string) {
	b.WriteString("tags:\n")
	for _, t := range tags {
		t = strings.TrimPrefix(strings.TrimSpace(t), "#")
		fmt.Fprintf(b, "  - \"#%s\"\n", t)
	}
}

// boolCandidates returns candidates only when unmatched — the body lists fallback
// links solely for the unmatched case.
func boolCandidates(unmatched bool, c []Candidate) []Candidate {
	if unmatched {
		return c
	}
	return nil
}

// --- duplicate detection ---------------------------------------------------

// DupIndex answers "does an existing tracked note already cover this?" against a
// snapshot of the vault's #Book and #LightNovel notes, keyed by resolved Link and
// by case-insensitive title. It is built by the orchestration (#75) from
// vault.ScanRoots and consulted before staging each item.
type DupIndex struct {
	byLink  map[string]string // lower(link) -> existing note basename
	byTitle map[string]string // dupKey(title) -> existing note basename
}

// NewDupIndex builds a DupIndex from existing tracked entries (ScanRoots already
// returns only #Book/#LightNovel notes with a Link).
func NewDupIndex(entries []vault.Entry) *DupIndex {
	d := &DupIndex{byLink: map[string]string{}, byTitle: map[string]string{}}
	for _, e := range entries {
		if l := strings.ToLower(strings.TrimSpace(e.Link)); l != "" {
			d.byLink[l] = e.Title
		}
		if k := dupKey(e.Title); k != "" {
			d.byTitle[k] = e.Title
		}
	}
	return d
}

// Lookup reports the basename of an existing note that a to-be-staged item
// duplicates — matched first by resolved Link (a real catalog/scrape URL; a
// synthetic unmatched link never collides), then by case-insensitive title.
func (d *DupIndex) Lookup(link, title string) (string, bool) {
	if l := strings.ToLower(strings.TrimSpace(link)); l != "" {
		if b, ok := d.byLink[l]; ok {
			return b, true
		}
	}
	if b, ok := d.byTitle[dupKey(title)]; ok {
		return b, true
	}
	return "", false
}

// dupKey normalizes a title for case-insensitive duplicate matching: lowercased,
// trimmed, internal whitespace collapsed.
func dupKey(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(title)), " ")
}

// --- helpers ---------------------------------------------------------------

// fmtIndex renders a Series Index without a trailing ".0" for whole numbers
// (1.0 -> "1", 2.5 -> "2.5"); a zero index renders blank.
func fmtIndex(f float64) string {
	if f == 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// writeNote writes content to path atomically through long-path-safe I/O, so a
// deep _volumes/<Series>/<long title>.md path can't blow the Windows MAX_PATH
// limit mid-import. The parent dir is created first.
func (w *Writer) writeNote(path, content string) error {
	if err := os.MkdirAll(longPath(filepath.Dir(path)), 0o755); err != nil {
		return err
	}
	return vault.AtomicWrite(longPath(path), []byte(content), 0o644)
}

// copyFile copies src to dst (both long-path-safe), creating dst's parent.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(longPath(filepath.Dir(dst)), 0o755); err != nil {
		return err
	}
	in, err := os.Open(longPath(src))
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(longPath(dst))
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// longPath prefixes an absolute Windows path with the `\\?\` extended-length
// marker so writes past the 260-char MAX_PATH limit succeed — the import's deep
// `_volumes/<Series>/<title>.md` paths (titles up to ~116 chars, series up to
// ~85) can easily exceed it on a OneDrive-synced vault. No-op off Windows, on an
// already-prefixed path, or when the path can't be made absolute.
func longPath(p string) string {
	if runtime.GOOS != "windows" || p == "" || strings.HasPrefix(p, `\\?\`) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	abs = filepath.Clean(abs)
	if strings.HasPrefix(abs, `\\`) { // UNC share: \\server\share -> \\?\UNC\server\share
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
	}
	return `\\?\` + abs
}
