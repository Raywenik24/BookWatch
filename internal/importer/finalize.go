// Finalize for the Calibre import (milestone 1.2.0, #76). Part 6 of 7.
//
// After the reviewer has looked over the staged notes in Obsidian and deleted
// whatever they didn't want, this moves every note that's *still on disk* out of
// the staging folder and into its real destination, dragging along the cover
// each surviving note references. It is deliberately disk-driven (not DB-driven):
// the reviewer's deletions are the source of truth, so a note the user removed is
// simply not there to move, and its now-orphan cover is left behind.
//
// Everything is collision-safe: a target that already exists is never
// overwritten — the note is skipped and reported. That's what protects the real
// series notes when an un-deleted #import/duplicate proposal would otherwise land
// on top of the note it duplicates.
package importer

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bookwatch/internal/vault"
)

// FinalizeDest are the absolute destination directories finalize moves staged
// notes + covers into. Volume notes keep their `_volumes/<Series>/` subpath
// under NoteDirLN; every LN cover (series + volume) lands flat in AttachDirLN,
// resolved by Obsidian's filename-only `![[embed]]` wherever it sits.
type FinalizeDest struct {
	NoteDirLN     string // tracked #LightNovel series notes (+ _volumes/ subtree)
	AttachDirLN   string // LN covers (series + volumes)
	NoteDirBook   string // tracked #Book notes (+ the inert #import/series index)
	AttachDirBook string // book covers
}

// FinalizeResult reports what a finalize pass did.
type FinalizeResult struct {
	Notes   int      `json:"notes"`   // notes moved to their destination
	Covers  int      `json:"covers"`  // covers moved alongside a note
	Skipped []string `json:"skipped"` // staging-relative notes left in place (target existed)
}

// coverEmbedRE pulls the cover filename out of a note's `![[cover_x.jpg]]` embed
// — the only `![[…]]` a staged note carries (the dup callout uses a plain
// `[[…]]` wikilink, so the leading `!` keeps them apart).
var coverEmbedRE = regexp.MustCompile(`!\[\[([^\]|#]+)\]\]`)

// Finalize moves every remaining staged note (and its referenced cover) from
// stagingDir into d. Notes are classified by their tags / path:
//   - a note under `_volumes/` → NoteDirLN (subpath preserved), cover → AttachDirLN
//   - a `#LightNovel` note      → NoteDirLN,  cover → AttachDirLN
//   - a `#Book` / `#import/series` note → NoteDirBook, cover → AttachDirBook
//
// The report note and any unrecognized `.md` are left untouched. Covers are only
// moved for notes that actually moved, so a deleted note's cover stays orphaned
// in staging rather than migrating on its own.
func Finalize(stagingDir string, d FinalizeDest) (FinalizeResult, error) {
	var res FinalizeResult
	root := longPath(stagingDir)

	// Collect notes first, then move — walking a tree we're moving files out of.
	var noteFiles []string
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // nothing staged yet — an empty/absent dir is fine
			}
			return err
		}
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			return nil
		}
		if e.Name() == reportName {
			return nil // the report stays behind with the staging folder
		}
		noteFiles = append(noteFiles, p)
		return nil
	})
	if err != nil {
		return res, err
	}

	for _, note := range noteFiles {
		if err := moveStagedNote(root, note, d, &res); err != nil {
			return res, err
		}
	}
	cleanupEmptyVolumeDirs(stagingDir)
	return res, nil
}

// FinalizeNotes moves a specific set of staged notes (+ their covers) out of
// stagingDir — the in-app reviewer's per-item / bulk-accept path, which finalizes
// only the notes the reviewer accepted rather than everything on disk. notePaths
// are absolute staged .md paths (a note plus, for an LN series, its volume
// notes). Same collision-safety and cover-follow rules as Finalize.
func FinalizeNotes(stagingDir string, notePaths []string, d FinalizeDest) (FinalizeResult, error) {
	var res FinalizeResult
	root := longPath(stagingDir)
	for _, note := range notePaths {
		if !strings.HasSuffix(strings.ToLower(note), ".md") {
			continue // covers travel with their note; skip non-note entries
		}
		if err := moveStagedNote(root, longPath(note), d, &res); err != nil {
			return res, err
		}
	}
	cleanupEmptyVolumeDirs(stagingDir)
	return res, nil
}

// cleanupEmptyVolumeDirs removes now-empty `_volumes/<Series>/` directories left
// behind once a series' volume notes have all been finalized (or rejected), plus
// the `_volumes/` parent itself once every series under it is gone. Best-effort:
// a dir that still has something in it (a note the reviewer hasn't dealt with
// yet, an orphaned cover) is simply left alone.
func cleanupEmptyVolumeDirs(stagingDir string) {
	volumesDir := longPath(filepath.Join(stagingDir, volumesSubdir))
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		return
	}
	allEmpty := true
	for _, e := range entries {
		if !e.IsDir() {
			allEmpty = false
			continue
		}
		sub := filepath.Join(volumesDir, e.Name())
		subEntries, err := os.ReadDir(sub)
		if err != nil || len(subEntries) > 0 {
			allEmpty = false // still has an unhandled volume note / orphan cover — leave it
			continue
		}
		removeEmptyDirWithRetry(sub)
	}
	if allEmpty {
		removeEmptyDirWithRetry(volumesDir)
	}
}

// removeEmptyDirWithRetry removes a dir just confirmed empty, retrying briefly on
// failure. A dir the notes/covers were just moved out of is intermittently held
// open by the OneDrive sync client right afterward, same as the write-lock
// RenameWithRetry works around; retrying turns that into a short wait instead of
// a permanent leftover. Only called on dirs already known to be empty, so this
// never burns the retry budget on a dir that's genuinely still occupied.
func removeEmptyDirWithRetry(dir string) {
	for i := 0; i < 12; i++ {
		if os.Remove(dir) == nil {
			return
		}
		time.Sleep(time.Duration(25*(i+1)) * time.Millisecond)
	}
}

// moveStagedNote moves one staged note (and the cover it embeds) from staging
// into its routed destination, accumulating into res. A target that already
// exists is skipped + reported (never overwritten). root is the long-path
// staging root; note is an absolute (long-path) staged .md path under it.
func moveStagedNote(root, note string, d FinalizeDest, res *FinalizeResult) error {
	rel, relErr := filepath.Rel(root, note)
	if relErr != nil {
		return relErr
	}
	content, readErr := os.ReadFile(note)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil // already moved/removed — nothing to do
		}
		return readErr
	}

	noteDir, attachDir, ok := classifyNote(rel, string(content), d)
	if !ok {
		return nil // unrecognized note — leave it in staging
	}

	var destNote string
	if underVolumes(rel) {
		destNote = filepath.Join(noteDir, rel) // preserve _volumes/<Series>/
	} else {
		destNote = filepath.Join(noteDir, filepath.Base(note))
	}

	moved, mErr := moveNoCollision(note, destNote)
	if mErr != nil {
		return mErr
	}
	if !moved {
		res.Skipped = append(res.Skipped, filepath.ToSlash(rel))
		return nil // target exists — leave note + cover in staging, report it
	}
	res.Notes++

	// Move the one cover this note embeds, from beside the note in staging into
	// the matching attachments dir.
	if m := coverEmbedRE.FindStringSubmatch(string(content)); m != nil {
		coverName := strings.TrimSpace(m[1])
		src := filepath.Join(filepath.Dir(note), coverName)
		movedCover, cErr := moveNoCollision(src, filepath.Join(attachDir, coverName))
		if cErr != nil {
			return cErr
		}
		if movedCover {
			res.Covers++
		}
	}
	return nil
}

// classifyNote returns the destination note + attachments dirs for a staged
// note, given its staging-relative path and content. ok is false for a note that
// doesn't look like anything the import writes (left in place). A volume note is
// recognized by its `_volumes/` path; the rest by their frontmatter tag.
func classifyNote(rel, content string, d FinalizeDest) (noteDir, attachDir string, ok bool) {
	switch {
	case underVolumes(rel):
		return d.NoteDirLN, d.AttachDirLN, true
	case strings.Contains(content, `"#LightNovel"`):
		return d.NoteDirLN, d.AttachDirLN, true
	case strings.Contains(content, `"#Book"`):
		return d.NoteDirBook, d.AttachDirBook, true
	case strings.Contains(content, `"#import/series"`):
		// Inert series-index companion of the #Book notes — keep it with them.
		return d.NoteDirBook, d.AttachDirBook, true
	default:
		return "", "", false
	}
}

// underVolumes reports whether a staging-relative path is a staged LN volume note
// (or its cover), i.e. lives under the `_volumes/` subtree.
func underVolumes(rel string) bool {
	return strings.HasPrefix(filepath.ToSlash(rel), volumesSubdir+"/")
}

// moveNoCollision moves src to dst without ever overwriting: if dst already
// exists it returns (false, nil) so the caller can skip + report. A missing src
// (e.g. a note references a cover that was never staged) is treated as "nothing
// moved" rather than an error. Long-path-safe for the deep _volumes tree.
func moveNoCollision(src, dst string) (bool, error) {
	if _, err := os.Stat(longPath(dst)); err == nil {
		return false, nil // target exists — never clobber
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if _, err := os.Stat(longPath(src)); err != nil {
		if os.IsNotExist(err) {
			return false, nil // nothing to move
		}
		return false, err
	}
	if err := os.MkdirAll(longPath(filepath.Dir(dst)), 0o755); err != nil {
		return false, err
	}
	if err := vault.RenameWithRetry(longPath(src), longPath(dst)); err != nil {
		return false, err
	}
	return true, nil
}
