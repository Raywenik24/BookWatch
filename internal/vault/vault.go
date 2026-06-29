// Package vault reads (and later writes) Obsidian notes. Notes are
// identified by the #LightNovel tag, not by folder. Phase 1 is read-only.
package vault

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Ported regexes from metadata_extractor.py.
var (
	linkRE           = regexp.MustCompile(`(?i)^Link:\s*(https?://\S+)`)
	volumesRE        = regexp.MustCompile(`(?i)^Volumes:\s*(\d+)`)
	coverRE          = regexp.MustCompile(`(?i)^Cover:\s*"?\[\[(.+?)\]\]"?`)
	lnTemplateRE     = regexp.MustCompile(`(?i)^Template_used:\s*LightNovelTemplate\s*$`)
	bookTemplateRE   = regexp.MustCompile(`(?i)^Template_used:\s*BookTemplate\s*$`)
	statusRE         = regexp.MustCompile(`(?i)^Status:\s*(.*)$`)
	statusListItemRE = regexp.MustCompile(`^\s*-\s*(.+)$`)
	readVolumesRE    = regexp.MustCompile(`(?i)^Read Volumes:\s*(\d+)`)
	authorRE         = regexp.MustCompile(`(?i)^Author:\s*(.+)$`)
	workIDRE         = regexp.MustCompile(`(?i)^Work ID:\s*(.+)$`)
	releasedENRE     = regexp.MustCompile(`(?i)^Released EN:\s*(.+)$`)

	// Prefix matchers for line-based rewriting.
	volPrefixRE = regexp.MustCompile(`(?i)^Volumes:\s*`)
	lastUpdRE   = regexp.MustCompile(`(?i)^Last Update:\s*`)
)

// Entry is one tracked note — either a light novel (Kind="ln") or a book
// (Kind="book"). Fields irrelevant to a kind are left at their zero value.
type Entry struct {
	Title          string
	Link           string
	Volumes        int
	Path           string
	Cover          string // attachment filename from `Cover: "[[file]]"` (no path)
	Status         string // e.g. "Queue", "Completed", "Dropped"
	ReadVolumes    int
	HasReadVolumes bool   // false when the field is blank/absent
	Kind           string // "ln" | "book"
	Author         string // book notes (and LN notes once the scraper adds it)
	WorkID         string // book notes — OpenLibrary work ID e.g. OL20749838W
	ReleasedEN     string // book notes — English release year/date
}

// Scan walks root for .md notes tagged #LightNovel that carry a Link,
// returning one Entry each. Folder-agnostic (matches the _LN_all.base view).
func Scan(root string) ([]Entry, error) {
	var entries []Entry

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// A traversal error on the root itself is fatal — there's nothing to
			// scan. Anything deeper (an unreadable dir/file, a OneDrive
			// files-on-demand placeholder that won't open) is logged and skipped
			// so one bad entry can't abort the whole check.
			if path == root {
				return err
			}
			log.Printf("vault scan: skipping %s: %v", path, err)
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}

		e, ok, perr := parse(path)
		if perr != nil {
			log.Printf("vault scan: skipping %s: %v", path, perr)
			return nil
		}
		if ok {
			entries = append(entries, e)
		}
		return nil
	})
	return entries, err
}

// parse reads the YAML frontmatter and returns the entry. ok=true only for
// notes tagged #LightNovel+LightNovelTemplate or #Book+BookTemplate with a Link.
func parse(path string) (Entry, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Entry{}, false, err
	}
	defer f.Close()

	var (
		link           string
		volumes        int
		cover          string
		status         string
		readVolumes    int
		hasReadVolumes bool
		pendingStatus  bool
		isLightNvl     bool
		hasLNTemplate  bool
		isBook         bool
		hasBookTempl   bool
		author         string
		workID         string
		releasedEN     string
		frontmatter    bool
		seenFence      bool
	)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()

		if strings.TrimSpace(line) == "---" {
			if !seenFence {
				seenFence = true
				frontmatter = true
				continue
			}
			break // end of frontmatter
		}
		if !frontmatter {
			continue
		}

		if pendingStatus {
			if m := statusListItemRE.FindStringSubmatch(line); m != nil {
				status = strings.TrimSpace(m[1])
			}
			pendingStatus = false
		}
		if strings.Contains(line, "#LightNovel") {
			isLightNvl = true
		}
		if strings.Contains(line, "#Book") {
			isBook = true
		}
		if lnTemplateRE.MatchString(line) {
			hasLNTemplate = true
		}
		if bookTemplateRE.MatchString(line) {
			hasBookTempl = true
		}
		if m := linkRE.FindStringSubmatch(line); m != nil {
			link = m[1]
		}
		if m := volumesRE.FindStringSubmatch(line); m != nil {
			volumes, _ = strconv.Atoi(m[1])
		}
		if m := coverRE.FindStringSubmatch(line); m != nil {
			cover = strings.TrimSpace(m[1])
		}
		if m := statusRE.FindStringSubmatch(line); m != nil {
			val := strings.TrimSpace(m[1])
			if val != "" {
				status = val
			} else {
				pendingStatus = true
			}
		}
		if m := readVolumesRE.FindStringSubmatch(line); m != nil {
			readVolumes, _ = strconv.Atoi(m[1])
			hasReadVolumes = true
		}
		if m := authorRE.FindStringSubmatch(line); m != nil {
			author = strings.TrimSpace(m[1])
		}
		if m := workIDRE.FindStringSubmatch(line); m != nil {
			workID = strings.TrimSpace(m[1])
		}
		if m := releasedENRE.FindStringSubmatch(line); m != nil {
			releasedEN = strings.TrimSpace(m[1])
		}
	}
	if err := sc.Err(); err != nil {
		return Entry{}, false, err
	}

	title := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	switch {
	case isLightNvl && hasLNTemplate && link != "":
		return Entry{
			Kind: "ln", Title: title, Link: link, Path: path,
			Volumes: volumes, Cover: cover, Status: status,
			ReadVolumes: readVolumes, HasReadVolumes: hasReadVolumes,
			Author: author,
		}, true, nil
	case isBook && hasBookTempl && link != "":
		return Entry{
			Kind: "book", Title: title, Link: link, Path: path,
			Cover: cover, Status: status,
			Author: author, WorkID: workID, ReleasedEN: releasedEN,
		}, true, nil
	default:
		return Entry{}, false, nil
	}
}

// Today returns the date stamp used for Last Update (YYYY-MM-DD).
func Today() string { return time.Now().Format("2006-01-02") }

// UpdateVolumes rewrites the `Volumes:` line and sets `Last Update:` inside the
// note's frontmatter. Line-based — every other line (and the original newline
// style) is preserved. Missing fields are inserted before the closing fence.
// It does NOT touch Status, Read Volumes, or the file location.
func UpdateVolumes(path string, newVolume int, today string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	nl := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	fence := 0
	closeIdx := -1
	wroteVol, wroteUpd := false, false

	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			fence++
			if fence == 2 {
				closeIdx = i
				break
			}
			continue
		}
		if fence == 1 {
			switch {
			case volPrefixRE.MatchString(line):
				lines[i] = fmt.Sprintf("Volumes: %d", newVolume)
				wroteVol = true
			case lastUpdRE.MatchString(line):
				lines[i] = "Last Update: " + today
				wroteUpd = true
			}
		}
	}

	if closeIdx == -1 {
		return fmt.Errorf("no frontmatter in %s", path)
	}
	if !wroteVol {
		lines = insertAt(lines, closeIdx, fmt.Sprintf("Volumes: %d", newVolume))
		closeIdx++
	}
	if !wroteUpd {
		lines = insertAt(lines, closeIdx, "Last Update: "+today)
	}

	return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
}

// UpdateStatus rewrites the `Status:` field inside the note's frontmatter,
// always writing in list format:
//
//	Status:
//	  - <newStatus>
//
// Both scalar (Status: Value) and list (Status:\n  - Value) forms are handled.
// If no Status field exists it is inserted before the closing fence.
func UpdateStatus(path, newStatus string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	nl := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	fence := 0
	closeIdx := -1
	statusIdx := -1
	statusIsScalar := false

	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			fence++
			if fence == 2 {
				closeIdx = i
				break
			}
			continue
		}
		if fence == 1 {
			if m := statusRE.FindStringSubmatch(line); m != nil && statusIdx == -1 {
				statusIdx = i
				statusIsScalar = strings.TrimSpace(m[1]) != ""
			}
		}
	}

	if closeIdx == -1 {
		return fmt.Errorf("no frontmatter in %s", path)
	}

	switch {
	case statusIdx != -1 && statusIsScalar:
		// "Status: Value" → "Status:" + "  - newStatus"
		lines[statusIdx] = "Status:"
		lines = insertAt(lines, statusIdx+1, "  - "+newStatus)
	case statusIdx != -1:
		// List form: next line is "  - OldValue"
		next := statusIdx + 1
		if next < len(lines) && statusListItemRE.MatchString(lines[next]) {
			lines[next] = "  - " + newStatus
		} else {
			lines = insertAt(lines, next, "  - "+newStatus)
		}
	default:
		// No Status field: insert key + list item before closing fence.
		lines = insertAt(lines, closeIdx, "Status:")
		lines = insertAt(lines, closeIdx+1, "  - "+newStatus)
	}

	return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
}

// AtomicWrite writes data to a temp file in the same directory and renames it
// over path, so a crash or error mid-write can never leave a half-written note
// (os.Rename replaces the destination atomically on the same filesystem).
// Shared with note creation so both write paths are crash-safe.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bw-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has moved it
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func insertAt(lines []string, idx int, val string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	out = append(out, val)
	out = append(out, lines[idx:]...)
	return out
}
