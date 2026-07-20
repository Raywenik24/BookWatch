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
	seriesRE         = regexp.MustCompile(`(?i)^Series:\s*(.*)$`)
	seriesIndexRE    = regexp.MustCompile(`(?i)^Series Index:\s*(.*)$`)
	seriesPrefixRE   = regexp.MustCompile(`(?i)^Series:\s*`)
	seriesIdxPrefRE  = regexp.MustCompile(`(?i)^Series Index:\s*`)
	authorPrefixRE   = regexp.MustCompile(`(?i)^Author:\s*`)
	tagsKeyRE        = regexp.MustCompile(`(?i)^tags:\s*(.*)$`)
	tagItemRE        = regexp.MustCompile(`^\s*-\s*"?#?([^"]+?)"?\s*$`)

	// Prefix matchers for line-based rewriting.
	volPrefixRE      = regexp.MustCompile(`(?i)^Volumes:\s*`)
	lastUpdRE        = regexp.MustCompile(`(?i)^Last Update:\s*`)
	coverPrefixRE    = regexp.MustCompile(`(?i)^Cover:\s*`)
	releasedPrefixRE = regexp.MustCompile(`(?i)^Released EN:\s*`)
	linkPrefixRE     = regexp.MustCompile(`(?i)^Link:\s*`)
	workIDPrefixRE   = regexp.MustCompile(`(?i)^Work ID:\s*`)
	titlePrefixRE    = regexp.MustCompile(`(?i)^Title:\s*`)
	readVolPrefixRE  = regexp.MustCompile(`(?i)^Read Volumes:\s*`)

	// Body structural lines (below the frontmatter): the H3 title, the cover
	// embed, and the LN "[[Light Novel]]" link. Used to separate the editable
	// description prose from the boilerplate the builders emit.
	h3RE     = regexp.MustCompile(`^###\s`)
	embedRE  = regexp.MustCompile(`^!\[\[(.+?)\]\]\s*$`)
	lnLinkRE = regexp.MustCompile(`^\[\[Light Novel\]\]\s*$`)
)

// Entry is one tracked note — either a light novel (Kind="ln") or a book
// (Kind="book"). Fields irrelevant to a kind are left at their zero value.
type Entry struct {
	Title          string
	Link           string
	Volumes        int
	Path           string
	Cover          string // attachment filename from `Cover: "[[file]]"` (no path)
	Status         string // e.g. "Backlog", "Completed", "Dropped"
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

// ResolvePath resolves p against vaultDir: an absolute p (e.g. a full
// "D:\..." path) is used as-is, so a setting can point outside the vault or
// just be pasted from Explorer; anything else is treated as relative to the
// vault root. Lets scan roots and new-note/attachments folders be entered
// either way.
func ResolvePath(vaultDir, p string) string {
	p = filepath.FromSlash(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(vaultDir, p)
}

// ScanRoots walks each of roots (Light Novel + Book scan roots, typically),
// dropping any root nested inside another so an overlapping pair isn't
// walked twice, and merges the results — deduped by absolute note path in
// case two configured roots turn out to be the same directory.
func ScanRoots(roots []string) ([]Entry, error) {
	var all []Entry
	seen := map[string]bool{}
	for _, root := range dedupeRoots(roots) {
		entries, err := Scan(root)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			abs, err := filepath.Abs(e.Path)
			if err != nil {
				abs = e.Path
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			all = append(all, e)
		}
	}
	return all, nil
}

// dedupeRoots drops blanks, exact duplicates, and any root nested inside
// another root in the list, so a Book path configured as a subfolder of the
// Light Novel path (or vice versa) doesn't get scanned twice.
func dedupeRoots(roots []string) []string {
	var abs []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		a, err := filepath.Abs(r)
		if err != nil {
			a = r
		}
		abs = append(abs, a)
	}
	var out []string
	for i, r := range abs {
		nested := false
		for j, other := range abs {
			if i == j {
				continue
			}
			if r == other {
				if j < i { // exact duplicate: keep the earliest occurrence
					nested = true
				}
				continue
			}
			if rel, err := filepath.Rel(other, r); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				nested = true
			}
		}
		if !nested {
			out = append(out, r)
		}
	}
	return out
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

// UpdateReadVolumes rewrites the `Read Volumes:` frontmatter line, inserting it
// before the closing fence if absent. It does not touch Volumes, Status, or
// Last Update — callers decide those separately (#67).
func UpdateReadVolumes(path string, newReadVolumes int) error {
	return setFrontmatterScalar(path, readVolPrefixRE, "Read Volumes", strconv.Itoa(newReadVolumes))
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

// loadFrontmatter reads a note, detects its newline style, splits it into lines
// (normalised to \n for indexing), and locates the closing `---` fence. Shared
// by the line-based frontmatter/body rewriters below so each doesn't re-open and
// re-scan the file.
func loadFrontmatter(path string) (lines []string, nl string, closeIdx int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", -1, err
	}
	nl = "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		nl = "\r\n"
	}
	lines = strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	fence := 0
	closeIdx = -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			fence++
			if fence == 2 {
				closeIdx = i
				break
			}
		}
	}
	if closeIdx == -1 {
		return nil, "", -1, fmt.Errorf("no frontmatter in %s", path)
	}
	return lines, nl, closeIdx, nil
}

// setFrontmatterScalar rewrites (or inserts before the closing fence) a
// "Key: value" frontmatter line.
func setFrontmatterScalar(path string, prefixRE *regexp.Regexp, key, value string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	wrote := false
	for i := 0; i < closeIdx; i++ {
		if prefixRE.MatchString(lines[i]) {
			lines[i] = key + ": " + value
			wrote = true
			break
		}
	}
	if !wrote {
		lines = insertAt(lines, closeIdx, key+": "+value)
	}
	return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
}

// UpdateReleasedEN rewrites the `Released EN:` frontmatter line (book notes).
func UpdateReleasedEN(path, value string) error {
	return setFrontmatterScalar(path, releasedPrefixRE, "Released EN", value)
}

// UpdateLink rewrites the `Link:` frontmatter line — used by the in-app import
// reviewer when a chosen candidate replaces the synthetic unmatched placeholder.
func UpdateLink(path, value string) error {
	return setFrontmatterScalar(path, linkPrefixRE, "Link", value)
}

// UpdateWorkID rewrites the `Work ID:` frontmatter line — set when the import
// reviewer resolves an unmatched note to a real catalog link.
func UpdateWorkID(path, value string) error {
	return setFrontmatterScalar(path, workIDPrefixRE, "Work ID", value)
}

// UpdateAuthor rewrites the `Author:` frontmatter line — editable in the reviewer.
func UpdateAuthor(path, value string) error {
	return setFrontmatterScalar(path, authorPrefixRE, "Author", value)
}

// UpdateSeries / UpdateSeriesIndex rewrite the `Series:` / `Series Index:`
// frontmatter lines — editable in the import reviewer.
func UpdateSeries(path, value string) error {
	return setFrontmatterScalar(path, seriesPrefixRE, "Series", value)
}

func UpdateSeriesIndex(path, value string) error {
	return setFrontmatterScalar(path, seriesIdxPrefRE, "Series Index", value)
}

// UpdateTags rewrites the `tags:` block list to exactly the given tags (each
// stored as `  - "#Tag"`, matching the builders). A nil/empty list leaves just
// the `tags:` key.
func UpdateTags(path string, tags []string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	block := []string{"tags:"}
	for _, t := range tags {
		t = strings.TrimPrefix(strings.TrimSpace(t), "#")
		if t == "" {
			continue
		}
		block = append(block, `  - "#`+t+`"`)
	}

	tagsIdx := -1
	for i := 0; i < closeIdx; i++ {
		if tagsKeyRE.MatchString(lines[i]) {
			tagsIdx = i
			break
		}
	}

	var out []string
	if tagsIdx == -1 {
		out = append(out, lines[:closeIdx]...)
		out = append(out, block...)
		out = append(out, lines[closeIdx:]...)
	} else {
		end := tagsIdx + 1
		for end < closeIdx && tagItemRE.MatchString(lines[end]) {
			end++
		}
		out = append(out, lines[:tagsIdx]...)
		out = append(out, block...)
		out = append(out, lines[end:]...)
	}
	return AtomicWrite(path, []byte(strings.Join(out, nl)), 0o644)
}

// SetTitleHeading rewrites the first body `### ` heading (inserting one if the
// body has none). Used by note rename so the in-body title tracks the filename.
func SetTitleHeading(path, title string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	for i := closeIdx + 1; i < len(lines); i++ {
		if h3RE.MatchString(strings.TrimSpace(lines[i])) {
			lines[i] = "### " + title
			return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
		}
	}
	lines = insertAt(lines, closeIdx+1, "### "+title)
	return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
}

// UpdateTitleField rewrites the `Title:` frontmatter line when the note has one
// (book notes do; LN notes key on `Series:` instead and are left untouched). A
// rename changes the filename — this keeps the in-frontmatter title in step.
func UpdateTitleField(path, title string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	for i := 0; i < closeIdx; i++ {
		if titlePrefixRE.MatchString(lines[i]) {
			lines[i] = "Title: " + title
			return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
		}
	}
	return nil
}

// UpdateCover points the note at a new cover attachment: it rewrites the
// `Cover: "[[name]]"` frontmatter field and the body `![[name]]` embed (adding
// the embed after the H3 title if the note had no cover before).
func UpdateCover(path, coverName string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	field := `Cover: "[[` + coverName + `]]"`
	wrote := false
	for i := 0; i < closeIdx; i++ {
		if coverPrefixRE.MatchString(lines[i]) {
			lines[i] = field
			wrote = true
			break
		}
	}
	if !wrote {
		lines = insertAt(lines, closeIdx, field)
		closeIdx++
	}

	embed := "![[" + coverName + "]]"
	found := false
	for i := closeIdx + 1; i < len(lines); i++ {
		if embedRE.MatchString(strings.TrimSpace(lines[i])) {
			lines[i] = embed
			found = true
			break
		}
	}
	if !found {
		ins := closeIdx + 1
		for i := closeIdx + 1; i < len(lines); i++ {
			if h3RE.MatchString(strings.TrimSpace(lines[i])) {
				ins = i + 1
				break
			}
		}
		lines = insertAt(lines, ins, "")
		lines = insertAt(lines, ins+1, embed)
	}
	return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
}

// UpdateDescription replaces the note's description prose (the body below the
// structural header lines) with newDesc, preserving the H3 title, cover embed,
// and LN "[[Light Novel]]" link exactly. Mirrors the body layout the builders
// emit: one blank line separates the header from the description.
func UpdateDescription(path, newDesc string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	// Description starts at the first non-structural, non-blank body line.
	j := closeIdx + 1
	for j < len(lines) {
		t := strings.TrimSpace(lines[j])
		if t == "" || h3RE.MatchString(t) || embedRE.MatchString(t) || lnLinkRE.MatchString(t) {
			j++
			continue
		}
		break
	}
	head := append([]string{}, lines[:j]...)
	for len(head) > 0 && strings.TrimSpace(head[len(head)-1]) == "" {
		head = head[:len(head)-1]
	}
	out := append(head, "") // single blank separator after the header
	if strings.TrimSpace(newDesc) != "" {
		out = append(out, strings.Split(strings.ReplaceAll(newDesc, "\r\n", "\n"), "\n")...)
	}
	out = append(out, "") // trailing newline
	return AtomicWrite(path, []byte(strings.Join(out, nl)), 0o644)
}

// notesHeading marks the personal-notes section a completion writes into the
// body of a volume/book note (#103). H2 so it sits below the H3 note title.
const notesHeading = "## Notes"

// NotesSection returns the prose under the body's `## Notes` heading (empty when
// the note has no such section). Read side of the completion-notes feature
// (#103): the edit-entry dialog loads it so the text can be revised.
func NotesSection(path string) (string, error) {
	lines, _, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return "", err
	}
	start := notesHeadingIdx(lines, closeIdx)
	if start == -1 {
		return "", nil
	}
	return strings.TrimSpace(strings.Join(lines[start+1:notesSectionEnd(lines, start)], "\n")), nil
}

// SetNotesSection inserts or replaces the body `## Notes` section with notesText
// (#103). The section is placed after the description and before a volume note's
// prev/next nav footer, so re-editing replaces it in place rather than appending.
// A blank notesText removes the section entirely.
func SetNotesSection(path, notesText string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	notesText = strings.TrimSpace(strings.ReplaceAll(notesText, "\r\n", "\n"))

	// Excise any existing section (and the blank separators just above it) first,
	// so both the replace and the remove paths start from a clean body.
	if start := notesHeadingIdx(lines, closeIdx); start != -1 {
		end := notesSectionEnd(lines, start)
		for start > closeIdx+1 && strings.TrimSpace(lines[start-1]) == "" {
			start--
		}
		out := append([]string{}, lines[:start]...)
		lines = append(out, lines[end:]...)
	}

	if notesText == "" {
		return AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
	}

	block := append([]string{"", notesHeading, ""}, strings.Split(notesText, "\n")...)
	var out []string
	if ins := navFooterIdx(lines, closeIdx); ins != -1 {
		block = append(block, "") // blank separator before the nav `---`
		out = append(out, lines[:ins]...)
		out = append(out, block...)
		out = append(out, lines[ins:]...)
	} else {
		// No nav footer (e.g. a #Book note): append at the end, past the description.
		end := len(lines)
		for end > closeIdx+1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		out = append(out, lines[:end]...)
		out = append(out, block...)
		out = append(out, "") // trailing newline
	}
	return AtomicWrite(path, []byte(strings.Join(out, nl)), 0o644)
}

// notesHeadingIdx returns the body line index of the `## Notes` heading (after
// the closing frontmatter fence at closeIdx), or -1 if there is none.
func notesHeadingIdx(lines []string, closeIdx int) int {
	for i := closeIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == notesHeading {
			return i
		}
	}
	return -1
}

// notesSectionEnd returns the exclusive end index of the `## Notes` section that
// opens at headingIdx: it runs until the next ATX heading, a `---` rule (the
// volume-nav footer), or EOF.
func notesSectionEnd(lines []string, headingIdx int) int {
	for i := headingIdx + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "---" || strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "### ") {
			return i
		}
	}
	return len(lines)
}

// SetVolumeNav replaces a volume note's prev/next reading-nav footer with
// footerLines (each a full line, e.g. "Previous: [[…]]" / "Next: [[…]]"). Passing
// no lines removes any existing footer (a volume with nothing left to link).
// Used when a later gap-fill adds volumes and an existing note's `Next:` link must
// be (re)written (#109): the frontmatter, cover, description, and `## Notes`
// section are all preserved — only the trailing footer changes.
func SetVolumeNav(path string, footerLines []string) error {
	lines, nl, closeIdx, err := loadFrontmatter(path)
	if err != nil {
		return err
	}
	// Drop an existing nav footer (its `---` rule to EOF), then trim the blank
	// separators above it, so the rewrite starts from a clean body tail.
	if idx := navFooterIdx(lines, closeIdx); idx != -1 {
		lines = lines[:idx]
	}
	for len(lines) > closeIdx+1 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	out := lines
	if len(footerLines) > 0 {
		out = append(out, "", "---")
		out = append(out, footerLines...)
	}
	out = append(out, "") // trailing newline
	return AtomicWrite(path, []byte(strings.Join(out, nl)), 0o644)
}

// navFooterIdx returns the index of a volume note's nav-footer `---` rule — the
// trailing `---` whose following non-blank lines are only Previous:/Next:
// wikilinks (written by the #LNVolume builder) — or -1 when the body has none.
func navFooterIdx(lines []string, closeIdx int) int {
	for i := len(lines) - 1; i > closeIdx; i-- {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if t != "" && !strings.HasPrefix(t, "Previous:") && !strings.HasPrefix(t, "Next:") {
				return -1 // last rule isn't a nav footer — nothing to insert before
			}
		}
		return i
	}
	return -1
}

// Note is the full contents of a tracked note — every frontmatter field the UI
// shows or edits, plus the description prose from the body (the structural
// lines — H3 title, cover embed, LN "[[Light Novel]]" link — stripped out).
type Note struct {
	Title          string   `json:"title"` // from the filename (rename target)
	Kind           string   `json:"kind"`  // "ln" | "book"
	Link           string   `json:"link"`
	Author         string   `json:"author"`
	WorkID         string   `json:"work_id"`
	ReleasedEN     string   `json:"released_en"`
	Series         string   `json:"series"`
	SeriesIndex    string   `json:"series_index"`
	Cover          string   `json:"cover"`
	Status         string   `json:"status"`
	Volumes        int      `json:"volumes"`
	ReadVolumes    int      `json:"read_volumes"`
	HasReadVolumes bool     `json:"-"`
	Tags           []string `json:"tags"`
	Description    string   `json:"description"`
}

// ReadNote reads a note's full frontmatter and description body. It's the
// read side of the note-edit feature (#55); Scan/parse only extract the fields
// the checker needs, so this is a separate, fuller pass over the same file.
func ReadNote(path string) (Note, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Note{}, err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	n := Note{Title: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))}
	fence := 0
	closeIdx := -1
	pendingStatus := false
	inTags := false
	var isLN, isBook, hasLNTempl, hasBookTempl bool

	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			fence++
			if fence == 2 {
				closeIdx = i
				break
			}
			continue
		}
		if fence != 1 {
			continue
		}

		// Tag list items sit under the `tags:` key until a non-item line.
		if inTags {
			if m := tagItemRE.FindStringSubmatch(line); m != nil {
				n.Tags = append(n.Tags, strings.TrimSpace(m[1]))
				continue
			}
			inTags = false
		}
		if pendingStatus {
			if m := statusListItemRE.FindStringSubmatch(line); m != nil {
				n.Status = strings.TrimSpace(m[1])
			}
			pendingStatus = false
		}

		if strings.Contains(line, "#LightNovel") {
			isLN = true
		}
		if strings.Contains(line, "#Book") {
			isBook = true
		}
		if lnTemplateRE.MatchString(line) {
			hasLNTempl = true
		}
		if bookTemplateRE.MatchString(line) {
			hasBookTempl = true
		}
		if m := linkRE.FindStringSubmatch(line); m != nil {
			n.Link = m[1]
		}
		if m := volumesRE.FindStringSubmatch(line); m != nil {
			n.Volumes, _ = strconv.Atoi(m[1])
		}
		if m := coverRE.FindStringSubmatch(line); m != nil {
			n.Cover = strings.TrimSpace(m[1])
		}
		if m := readVolumesRE.FindStringSubmatch(line); m != nil {
			n.ReadVolumes, _ = strconv.Atoi(m[1])
			n.HasReadVolumes = true
		}
		if m := authorRE.FindStringSubmatch(line); m != nil {
			n.Author = strings.TrimSpace(m[1])
		}
		if m := workIDRE.FindStringSubmatch(line); m != nil {
			n.WorkID = strings.TrimSpace(m[1])
		}
		if m := releasedENRE.FindStringSubmatch(line); m != nil {
			n.ReleasedEN = strings.TrimSpace(m[1])
		}
		if m := seriesIndexRE.FindStringSubmatch(line); m != nil {
			n.SeriesIndex = strings.TrimSpace(m[1])
		} else if m := seriesRE.FindStringSubmatch(line); m != nil {
			n.Series = strings.TrimSpace(m[1])
		}
		if m := statusRE.FindStringSubmatch(line); m != nil {
			if v := strings.TrimSpace(m[1]); v != "" {
				n.Status = v
			} else {
				pendingStatus = true
			}
		}
		if tagsKeyRE.MatchString(line) {
			// `tags:` opens a list; any inline value (`tags: [..]`) is ignored —
			// notes here always use the block-list form the builders emit.
			inTags = true
		}
	}

	// The tag list items are consumed above (with `continue`), so also derive the
	// kind flags from the collected tags, not just an inline substring.
	for _, tg := range n.Tags {
		switch strings.ToLower(tg) {
		case "lightnovel":
			isLN = true
		case "book":
			isBook = true
		}
	}
	switch {
	case isBook && hasBookTempl:
		n.Kind = "book"
	case isLN && hasLNTempl:
		n.Kind = "ln"
	}

	if closeIdx != -1 {
		n.Description = extractDescription(lines[closeIdx+1:])
	}
	return n, nil
}

// extractDescription drops the structural body lines the builders emit (leading
// blanks, the `### title`, the `![[cover]]` embed, and the `[[Light Novel]]`
// link) and returns the remaining prose, trimmed of surrounding blank lines.
func extractDescription(body []string) string {
	i := 0
	for i < len(body) {
		t := strings.TrimSpace(body[i])
		if t == "" || h3RE.MatchString(t) || embedRE.MatchString(t) || lnLinkRE.MatchString(t) {
			i++
			continue
		}
		break
	}
	desc := body[i:]
	// Trim trailing blank lines.
	for len(desc) > 0 && strings.TrimSpace(desc[len(desc)-1]) == "" {
		desc = desc[:len(desc)-1]
	}
	return strings.Join(desc, "\n")
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
	return RenameWithRetry(tmpName, path)
}

// RenameWithRetry renames oldpath→newpath, retrying briefly on failure. On
// Windows a note living in a OneDrive-synced folder is intermittently locked by
// the sync client (or an on-access AV scan) right after it's written, so the
// rename fails with "Access is denied" / a sharing violation that clears within
// a few hundred milliseconds. Retrying turns that transient failure — which the
// user otherwise had to work around by waiting and re-saving — into a short wait.
func RenameWithRetry(oldpath, newpath string) error {
	var err error
	for i := 0; i < 12; i++ {
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
		time.Sleep(time.Duration(25*(i+1)) * time.Millisecond)
	}
	return err
}

func insertAt(lines []string, idx int, val string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	out = append(out, val)
	out = append(out, lines[idx:]...)
	return out
}
