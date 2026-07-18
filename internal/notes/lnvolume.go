// Per-volume #LNVolume note creation for the volume-note backfill (#90).
//
// When an LN series is added, a background job (server side) scrapes each volume
// from jnovels and writes one untracked #LNVolume note per volume here, under
// `<LN new note dir>/_volumes/<Series>/`, mirroring the Calibre import's volume
// notes so Currently-reading / Queue can show the *read volume's* own cover. A
// volume the scrape can't resolve gets a minimal placeholder tagged
// `#LNVolume/incomplete` (the default "make an empty note to fill later").
package notes

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"bookwatch/internal/vault"
)

// volumesSubdir mirrors the Calibre import's staging layout: untracked volume
// notes live under `_volumes/<Series>/` beneath the LN note dir, so the whole
// series moves as one and the folder is easy to exclude from scans.
const volumesSubdir = "_volumes"

// LNVolumeTitle is the display/file title of a backfilled volume note — the
// series name plus its volume number. Kept in one place so the reading-view
// cover lookup and the writer agree on naming.
func LNVolumeTitle(series string, volume int) string {
	return fmt.Sprintf("%s Volume %d", strings.TrimSpace(series), volume)
}

// VolumeNotePath returns the on-disk path of a series' `_volumes/<Series>/`
// directory, given the series note's own path (its sibling). The series note
// basename (sans .md) is the `_volumes` subfolder name.
func VolumeDir(seriesNotePath string) string {
	base := strings.TrimSuffix(filepath.Base(seriesNotePath), filepath.Ext(seriesNotePath))
	return filepath.Join(filepath.Dir(seriesNotePath), volumesSubdir, base)
}

// VolumePath returns the on-disk path of a specific volume note under a series.
func VolumePath(seriesNotePath, series string, volume int) string {
	return filepath.Join(VolumeDir(seriesNotePath), Sanitize(LNVolumeTitle(series, volume), false)+".md")
}

// incompleteTag is the marker a placeholder volume note carries; noteIncomplete
// tests for it (case-insensitively, with or without the leading '#').
const incompleteTag = "lnvolume/incomplete"

func noteIncomplete(n vault.Note) bool {
	for _, t := range n.Tags {
		if strings.Contains(strings.ToLower(t), incompleteTag) {
			return true
		}
	}
	return false
}

// VolumeState reports one volume's backfill status for the reviewer UI.
type VolumeState struct {
	Volume int    `json:"volume"`
	Title  string `json:"title"`
	State  string `json:"state"` // "resolved" | "incomplete" | "missing"
	Link   string `json:"link"`
}

// VolumeStates reports the status of volumes 1..volumes for a series: a
// #LNVolume/incomplete note is "incomplete", any other existing note is
// "resolved", and an absent note is "missing".
func VolumeStates(seriesNotePath, series string, volumes int) []VolumeState {
	out := make([]VolumeState, 0, volumes)
	for v := 1; v <= volumes; v++ {
		vs := VolumeState{Volume: v, Title: LNVolumeTitle(series, v), State: "missing"}
		if n, err := vault.ReadNote(longPath(VolumePath(seriesNotePath, series, v))); err == nil {
			if noteIncomplete(n) {
				vs.State = "incomplete"
			} else {
				vs.State = "resolved"
			}
			vs.Link = n.Link
		}
		out = append(out, vs)
	}
	return out
}

// VolumeStateOf reports one volume's status: "resolved", "incomplete", or
// "missing" (no note yet).
func VolumeStateOf(seriesNotePath, series string, volume int) string {
	n, err := vault.ReadNote(longPath(VolumePath(seriesNotePath, series, volume)))
	if err != nil {
		return "missing"
	}
	if noteIncomplete(n) {
		return "incomplete"
	}
	return "resolved"
}

// RemoveIncompleteVolumes deletes every #LNVolume/incomplete placeholder note in
// a series' `_volumes/<Series>/` folder, returning how many were removed. Used
// before a retroactive backfill so the freed slots are re-attempted while
// resolved (and hand-edited) notes are left untouched. A missing folder is not an
// error (nothing to remove).
func RemoveIncompleteVolumes(seriesNotePath string) (int, error) {
	volDir := VolumeDir(seriesNotePath)
	entries, err := os.ReadDir(longPath(volDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		p := filepath.Join(volDir, e.Name())
		n, rerr := vault.ReadNote(longPath(p))
		if rerr != nil || !noteIncomplete(n) {
			continue
		}
		if os.Remove(longPath(p)) == nil {
			removed++
		}
	}
	return removed, nil
}

// BuildLNVolumeNote renders an untracked #LNVolume note: series/index/language
// frontmatter, the volume's own jnovels Link, the cover embed, and the
// description. incomplete flips the tag to `#LNVolume/incomplete` and (by
// convention) is called with a blank link/cover/description when the jnovels
// lookup missed.
func BuildLNVolumeNote(series string, volume, total int, language, link, coverName, releasedEN, description, today string, incomplete bool) string {
	title := Sanitize(LNVolumeTitle(series, volume), false)
	tag := "#LNVolume"
	if incomplete {
		tag = "#LNVolume/incomplete"
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Title: %s\n", title)
	// Series is a wikilink property so Obsidian links the volume note back to its
	// series note (graph + backlinks + a clickable property), not just plain text.
	fmt.Fprintf(&b, "Series: \"[[%s]]\"\n", Sanitize(series, false))
	fmt.Fprintf(&b, "Series Index: %d\n", volume)
	fmt.Fprintf(&b, "Link: %s\n", link)
	fmt.Fprintf(&b, "Language: %s\n", language)
	if strings.TrimSpace(releasedEN) != "" {
		fmt.Fprintf(&b, "Released EN: %s\n", releasedEN)
	}
	if coverName != "" {
		fmt.Fprintf(&b, "Cover: \"[[%s]]\"\n", coverName)
	} else {
		b.WriteString("Cover:\n")
	}
	b.WriteString("tags:\n")
	fmt.Fprintf(&b, "  - \"%s\"\n", tag)
	fmt.Fprintf(&b, "created: %s\n", today)
	fmt.Fprintf(&b, "modified: %s\n", today)
	b.WriteString("---\n")
	fmt.Fprintf(&b, "### %s\n\n", title)
	if coverName != "" {
		fmt.Fprintf(&b, "![[%s]]\n\n", coverName)
	}
	if s := strings.TrimSpace(description); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}
	writeVolumeNav(&b, series, volume, total)
	return b.String()
}

// writeVolumeNav appends the reading-navigation footer: prev/next volume links
// (only those that exist within 1..total). The series link lives in the Series
// frontmatter property, so it isn't repeated here.
func writeVolumeNav(b *strings.Builder, series string, volume, total int) {
	if volume <= 1 && (total <= 0 || volume >= total) {
		return // nothing to link (a lone volume)
	}
	b.WriteString("\n---\n")
	if volume > 1 {
		fmt.Fprintf(b, "Previous: [[%s]]\n", Sanitize(LNVolumeTitle(series, volume-1), false))
	}
	if total > 0 && volume < total {
		fmt.Fprintf(b, "Next: [[%s]]\n", Sanitize(LNVolumeTitle(series, volume+1), false))
	}
}

// CreateLNVolume writes one #LNVolume note under the series' `_volumes/<Series>/`
// folder, downloading its cover into the LN attachments dir (so the same
// filename-only `![[embed]]` resolves that every other cover uses). A note that
// already exists is left untouched (ErrNoteExists) so a re-run never clobbers a
// hand-edited volume note. When incomplete is true (a missed scrape), a minimal
// placeholder is written with no cover. Returns the created note path + cover.
func CreateLNVolume(o Options, series string, volume, total int, language, link, coverURL, releasedEN, description string, incomplete bool) (Result, error) {
	displayTitle := LNVolumeTitle(series, volume)
	sanTitle := Sanitize(displayTitle, false)
	today := time.Now().Format("2006-01-02")

	volDir := filepath.Join(vault.ResolvePath(o.VaultDir, o.NewNoteDir), volumesSubdir, Sanitize(series, false))
	notePath := filepath.Join(volDir, sanTitle+".md")
	if _, err := os.Stat(longPath(notePath)); err == nil {
		return Result{}, fmt.Errorf("%w: %s.md", ErrNoteExists, sanTitle)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Result{}, err
	}

	coverName := ""
	if !incomplete && coverURL != "" {
		coverName = CoverName(displayTitle, coverExt(coverURL))
		attachAbs := vault.ResolvePath(o.VaultDir, o.AttachmentsDir)
		if err := os.MkdirAll(longPath(attachAbs), 0o755); err != nil {
			return Result{}, err
		}
		if err := download(coverURL, longPath(filepath.Join(attachAbs, coverName))); err != nil {
			// A cover that won't download shouldn't lose the whole note — fall back
			// to a note without one (still better than nothing to read next).
			coverName = ""
		}
	}

	content := BuildLNVolumeNote(series, volume, total, language, link, coverName, releasedEN, description, today, incomplete)
	if err := os.MkdirAll(longPath(volDir), 0o755); err != nil {
		return Result{}, err
	}
	if err := vault.AtomicWrite(longPath(notePath), []byte(content), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Path: notePath, Title: sanTitle, Volumes: volume, Cover: coverName}, nil
}

// SaveLNVolume writes a resolved #LNVolume note with an already-saved cover
// attachment (coverName may be "" for none), overwriting any note at that path.
// Unlike CreateLNVolume it neither downloads a cover nor refuses an existing note
// — it's the manual-fill path, where the caller has already saved the cover
// (upload or download) and decided to replace the placeholder.
func SaveLNVolume(o Options, series string, volume, total int, language, link, releasedEN, description, coverName string) (Result, error) {
	sanTitle := Sanitize(LNVolumeTitle(series, volume), false)
	today := time.Now().Format("2006-01-02")
	volDir := filepath.Join(vault.ResolvePath(o.VaultDir, o.NewNoteDir), volumesSubdir, Sanitize(series, false))
	notePath := filepath.Join(volDir, sanTitle+".md")
	content := BuildLNVolumeNote(series, volume, total, language, link, coverName, releasedEN, description, today, false)
	if err := os.MkdirAll(longPath(volDir), 0o755); err != nil {
		return Result{}, err
	}
	if err := vault.AtomicWrite(longPath(notePath), []byte(content), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Path: notePath, Title: sanTitle, Volumes: volume, Cover: coverName}, nil
}

// volumeLinksHeading marks the wikilink section AppendVolumeLinks adds, and
// doubles as the idempotency guard (a note that already has it is left alone).
const volumeLinksHeading = "### Volumes"

// AppendVolumeLinks adds a "### Volumes" section of `[[<Series> Volume N]]`
// wikilinks to the series note's body (after the description), so the tracked
// series note points at each backfilled volume note. Idempotent: a note that
// already carries the section is left untouched, so a re-run doesn't duplicate
// it. A no-op for a non-positive volume count.
func AppendVolumeLinks(seriesNotePath, series string, volumes int) error {
	if volumes <= 0 {
		return nil
	}
	data, err := os.ReadFile(longPath(seriesNotePath))
	if err != nil {
		return err
	}
	body := string(data)
	if strings.Contains(body, volumeLinksHeading) {
		return nil // already added
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n\n" + volumeLinksHeading + "\n\n")
	for k := 1; k <= volumes; k++ {
		fmt.Fprintf(&b, "- [[%s]]\n", Sanitize(LNVolumeTitle(series, k), false))
	}
	return vault.AtomicWrite(longPath(seriesNotePath), []byte(b.String()), 0o644)
}

// longPath prefixes an absolute Windows path with the `\\?\` extended-length
// marker so writes past the 260-char MAX_PATH limit succeed — a backfilled
// `_volumes/<Series>/<Series> Volume N.md` on a OneDrive-synced vault can be
// deep. No-op off Windows, on an already-prefixed path, or when the path can't
// be made absolute. Mirrors the Calibre importer's own long-path helper.
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
