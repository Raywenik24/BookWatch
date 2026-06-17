// Package vault reads (and later writes) Obsidian notes. Notes are
// identified by the #LightNovel tag, not by folder. Phase 1 is read-only.
package vault

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Ported regexes from metadata_extractor.py.
var (
	linkRE    = regexp.MustCompile(`(?i)^Link:\s*(https?://\S+)`)
	volumesRE = regexp.MustCompile(`(?i)^Volumes:\s*(\d+)`)

	// Prefix matchers for line-based rewriting.
	volPrefixRE = regexp.MustCompile(`(?i)^Volumes:\s*`)
	lastUpdRE   = regexp.MustCompile(`(?i)^Last Update:\s*`)
)

// Entry is one tracked novel.
type Entry struct {
	Title   string
	Link    string
	Volumes int
	Path    string
}

// Scan walks root for .md notes tagged #LightNovel that carry a Link,
// returning one Entry each. Folder-agnostic (matches the _LN_all.base view).
func Scan(root string) ([]Entry, error) {
	var entries []Entry

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}

		e, ok, perr := parse(path)
		if perr != nil {
			return perr
		}
		if ok {
			entries = append(entries, e)
		}
		return nil
	})
	return entries, err
}

// parse reads the YAML frontmatter. ok=true only when the note is tagged
// #LightNovel and has a Link.
func parse(path string) (Entry, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Entry{}, false, err
	}
	defer f.Close()

	var (
		link        string
		volumes     int
		isLightNvl  bool
		frontmatter bool
		seenFence   bool
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

		if strings.Contains(line, "#LightNovel") {
			isLightNvl = true
		}
		if m := linkRE.FindStringSubmatch(line); m != nil {
			link = m[1]
		}
		if m := volumesRE.FindStringSubmatch(line); m != nil {
			volumes, _ = strconv.Atoi(m[1])
		}
	}
	if err := sc.Err(); err != nil {
		return Entry{}, false, err
	}

	if !isLightNvl || link == "" {
		return Entry{}, false, nil
	}

	title := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return Entry{Title: title, Link: link, Volumes: volumes, Path: path}, true, nil
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

	return os.WriteFile(path, []byte(strings.Join(lines, nl)), 0o644)
}

func insertAt(lines []string, idx int, val string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	out = append(out, val)
	out = append(out, lines[idx:]...)
	return out
}
