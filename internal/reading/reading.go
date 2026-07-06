// Package reading is the engine behind the unified completed-reads log
// (`_Read.md`, issue #63). That note stays the canonical, append-only history
// database — one row per completed read, covering both Light Novel volumes and
// whole #Book works. This package reads it leniently (the note has grown a few
// quirks by hand — see Parse), counts re-reads, and appends new completions as
// compact rows without disturbing a single existing byte.
package reading

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"bookwatch/internal/vault"
)

// Read is one completed-read row. Every field is the raw cell text (trimmed);
// LN rows carry a Volume, #Book rows leave it blank. Dates are the log's own
// `YYYY.MM.DD` form (or blank — the log tolerates unknown/`----` cells).
type Read struct {
	YearMonth string `json:"year_month"` // "202607" — may be blank
	Title     string `json:"title"`      // inner text of the [[wikilink]]
	Volume    string `json:"volume"`     // "10" for LN, "" for #Book
	Start     string `json:"start"`      // "2026.07.06" or ""
	End       string `json:"end"`        // "2026.07.06" or ""
}

// titleRE pulls the note basename out of a `[[Title]]` cell. Non-greedy so a
// stray `]]` later in the row can't over-match.
var titleRE = regexp.MustCompile(`\[\[([^\]]+)\]\]`)

// separatorRow bootstraps a table when the log has none yet. The right-aligned
// volume column (`---:`) matches the hand-authored note's own separator.
const separatorRow = "| ------ | ------ | ---: | ---------- | ---------- |"

// ParseFile reads and parses the log at path. A missing file is not an error —
// there's simply nothing read yet, so it returns an empty slice.
func ParseFile(path string) ([]Read, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(raw), nil
}

// Parse extracts the completed reads from a `_Read.md` body, tolerating every
// quirk the hand-maintained note has accreted: YAML frontmatter and prose above
// the table, the first data row doubling as the markdown header (so there's no
// separate header to skip — the `|---|` separator row is dropped because it has
// no `[[link]]`), `----` placeholder date cells, heavy space-padding, trailing
// all-blank spare rows, and curly punctuation in titles. Any table row without a
// `[[wikilink]]` (the separator, blanks, stray prose) is skipped.
func Parse(raw []byte) []Read {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	lines = stripFrontmatter(lines)

	var out []Read
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "|") {
			continue
		}
		cells := splitRow(t)
		if len(cells) < 2 {
			continue
		}
		m := titleRE.FindStringSubmatch(cells[1])
		if m == nil {
			continue // separator / blank spare row / non-read row
		}
		r := Read{Title: strings.TrimSpace(m[1]), YearMonth: cells[0]}
		if len(cells) > 2 {
			r.Volume = cells[2]
		}
		if len(cells) > 3 {
			r.Start = cleanDate(cells[3])
		}
		if len(cells) > 4 {
			r.End = cleanDate(cells[4])
		}
		out = append(out, r)
	}
	return out
}

// stripFrontmatter drops a leading `---`…`---` YAML block (ignoring blank lines
// before it) so the table parse below never trips on frontmatter values.
func stripFrontmatter(lines []string) []string {
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "---" {
		return lines
	}
	for j := i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "---" {
			return lines[j+1:]
		}
	}
	return lines
}

// splitRow splits a `| a | b | c |` row into its trimmed cells, dropping the
// leading/trailing pipe delimiters.
func splitRow(t string) []string {
	t = strings.TrimPrefix(strings.TrimSpace(t), "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// cleanDate normalizes a date cell: a dash-only placeholder (`----`) or an empty
// cell both mean "unknown" and collapse to "". Anything else is kept verbatim.
func cleanDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Trim(s, "-") == "" {
		return ""
	}
	return s
}

// Key identifies a distinct read unit for re-read counting: an LN volume is
// (title, volume); a #Book is (title, ""). Every duplicate row for the same key
// is one more read of that unit.
type Key struct {
	Title  string
	Volume string
}

// ReReadCounts tallies how many times each (title, volume) unit was read. A
// count of 2+ is a re-read.
func ReReadCounts(reads []Read) map[Key]int {
	m := make(map[Key]int, len(reads))
	for _, r := range reads {
		m[Key{r.Title, r.Volume}]++
	}
	return m
}

// CountFor returns how many times (title, volume) appears in reads — the value
// a `×N` re-read badge shows (surfaced by the UI only at N ≥ 2).
func CountFor(reads []Read, title, volume string) int {
	return ReReadCounts(reads)[Key{strings.TrimSpace(title), strings.TrimSpace(volume)}]
}

// CompletedRow is a resolved, ready-to-write completion. Build it with
// NewCompletedRow so the known/unknown-date rules are applied once, centrally.
type CompletedRow struct {
	YearMonth string
	Title     string // note basename (rendered as [[Title]])
	Volume    string // "" for #Book
	Start     string
	End       string
}

// NewCompletedRow applies the completion date rules. When unknown ("I don't
// remember"), both date cells AND the YYYYMM cell are left blank — the read is
// still logged, just undated. Otherwise the dates are normalized to the log's
// `YYYY.MM.DD` form and YYYYMM is auto-filled from the end date (falling back to
// the start date when only that is known).
func NewCompletedRow(title, volume, start, end string, unknown bool) CompletedRow {
	row := CompletedRow{
		Title:  strings.TrimSpace(title),
		Volume: strings.TrimSpace(volume),
	}
	if unknown {
		return row
	}
	row.Start = normalizeDate(start)
	row.End = normalizeDate(end)
	if row.YearMonth = yearMonth(row.End); row.YearMonth == "" {
		row.YearMonth = yearMonth(row.Start)
	}
	return row
}

// dateLayouts are the forms NormalizeDate accepts: the HTML date picker's
// `YYYY-MM-DD` and the log's own `YYYY.MM.DD`.
var dateLayouts = []string{"2006-01-02", "2006.01.02"}

// normalizeDate reformats a recognized date to the log's `YYYY.MM.DD`. An empty
// cell stays empty; an unrecognized value is passed through untouched rather
// than dropped, so a hand-typed oddity is still recorded.
func normalizeDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if t, ok := parseDate(s); ok {
		return t.Format("2006.01.02")
	}
	return s
}

// yearMonth derives the `YYYYMM` stamp from a date cell, or "" if it isn't a
// recognizable date.
func yearMonth(s string) string {
	if t, ok := parseDate(s); ok {
		return t.Format("200601")
	}
	return ""
}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// render formats a compact table row. Blank cells stay blank (`|  |`); Obsidian
// renders it fine and it stays visually distinct from the padded legacy rows.
func (r CompletedRow) render() string {
	return fmt.Sprintf("| %s | [[%s]] | %s | %s | %s |", r.YearMonth, r.Title, r.Volume, r.Start, r.End)
}

// AppendCompleted appends row to the log at path as a single compact line,
// inserted right after the last populated table row (i.e. before any trailing
// blank spare rows). Every existing row is left byte-for-byte untouched — the
// write only adds one line. If the file has no table yet, a header row +
// separator are bootstrapped so the note renders as a table in Obsidian. The
// write is atomic with OneDrive lock-retry (via vault.AtomicWrite).
func AppendCompleted(path string, row CompletedRow) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	nl := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	newRow := row.render()

	insert := -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "|") && !isBlankRow(t) {
			insert = i
		}
	}

	var out []string
	if insert == -1 {
		out = bootstrap(lines, newRow)
	} else {
		out = make([]string, 0, len(lines)+1)
		out = append(out, lines[:insert+1]...)
		out = append(out, newRow)
		out = append(out, lines[insert+1:]...)
	}
	return vault.AtomicWrite(path, []byte(strings.Join(out, nl)), 0o644)
}

// isBlankRow reports whether a table row is an all-empty spare row (`|  |  |`).
// The separator row (`| --- | --- |`) is NOT blank, so it still anchors the
// insertion point when the log holds only a bootstrapped header + separator.
func isBlankRow(t string) bool {
	for _, c := range splitRow(t) {
		if c != "" {
			return false
		}
	}
	return true
}

// bootstrap builds a fresh table at the end of an otherwise table-less file:
// the new row (which, per the note's convention, doubles as the header) followed
// by a separator so Obsidian renders it. Trailing blank lines are collapsed and
// a single blank line separates the table from any preceding prose.
func bootstrap(lines []string, newRow string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	return append(lines, newRow, separatorRow, "")
}
