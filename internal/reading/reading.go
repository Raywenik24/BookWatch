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
	"path/filepath"
	"regexp"
	"strconv"
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
	Abandoned bool   `json:"abandoned"`  // end cell was a `----` marker: started, not finished
}

// titleRE pulls the note basename out of a `[[Title]]` cell. Non-greedy so a
// stray `]]` later in the row can't over-match.
var titleRE = regexp.MustCompile(`\[\[([^\]]+)\]\]`)

// separatorRow bootstraps a table when the log has none yet. The right-aligned
// volume column (`---:`) matches the hand-authored note's own separator.
const separatorRow = "| ------ | ------ | ---: | ---------- | ---------- |"

// headerRow labels the columns of a brand-new log created by the vault wizard
// (issue #65). The hand-maintained note lets its first data row double as the
// header, but a wizard-created log has no reads yet, so it gets an explicit
// header above the separator instead.
const headerRow = "| Month | Title | Vol | Start | End |"

// EnsureLog creates an empty reading log at path — a header row + separator so
// the note renders as a table in Obsidian — but only if the file doesn't
// already exist; an existing log (even an empty one) is left byte-for-byte
// untouched. Parent directories are created as needed. Used by the vault setup
// wizard's final confirm.
func EnsureLog(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already exists — never clobber
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return vault.AtomicWrite(path, []byte(headerRow+"\n"+separatorRow+"\n"), 0o644)
}

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
			// A `----` placeholder in the END cell is the abandoned marker
			// (started, never finished, not continued) — distinct from a blank
			// cell meaning "date unknown". cleanDate collapses both to "", so the
			// distinction is captured here before it's lost.
			r.Abandoned = isDashPlaceholder(cells[4])
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

// isDashPlaceholder reports whether a cell is a non-empty run of dashes (`----`),
// the log's abandoned-read marker in the end column.
func isDashPlaceholder(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && strings.Trim(s, "-") == ""
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

// MaxVolume returns the highest numeric volume logged for title (case-sensitive
// on the note basename), or 0 if the title has no numeric-volume rows yet. A
// non-numeric volume cell (a whole #Book leaves it blank) doesn't count.
func MaxVolume(reads []Read, title string) int {
	title = strings.TrimSpace(title)
	max := 0
	for _, r := range reads {
		if strings.TrimSpace(r.Title) != title {
			continue
		}
		if v, err := strconv.Atoi(strings.TrimSpace(r.Volume)); err == nil && v > max {
			max = v
		}
	}
	return max
}

// NextVolume suggests the next volume to read for an LN title: one past the
// highest volume already logged, clamped to [1, cap] when cap (the note's total
// Volumes) is known. With no cap it just returns max+1.
func NextVolume(reads []Read, title string, cap int) int {
	n := MaxVolume(reads, title) + 1
	if n < 1 {
		n = 1
	}
	if cap > 0 && n > cap {
		n = cap
	}
	return n
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

// abandonedMarker is what an abandoned read writes in its end cell: started, not
// finished, not continued. It renders literally (unlike a blank "unknown" cell)
// so the log stays human-readable, and Parse reads it back as Abandoned.
const abandonedMarker = "----"

// NewAbandonedRow builds a row for a read the user gave up on. The start date is
// kept (normalized, or blank if unknown) and the end cell carries the `----`
// marker; YYYYMM is derived from the start date. Unlike a completion this never
// implies the unit was finished.
func NewAbandonedRow(title, volume, start string) CompletedRow {
	return CompletedRow{
		YearMonth: yearMonth(normalizeDate(start)),
		Title:     strings.TrimSpace(title),
		Volume:    strings.TrimSpace(volume),
		Start:     normalizeDate(start),
		End:       abandonedMarker,
	}
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

// UpdateReadAt rewrites the index-th completed-read row (in file order, the same
// order Parse returns) to row, leaving every other line untouched. expectTitle
// guards against a stale index: if the row at index no longer names that title
// (the log shifted since the caller read it), it errors instead of editing the
// wrong entry. The write is atomic with OneDrive lock-retry.
func UpdateReadAt(path string, index int, expectTitle string, row CompletedRow) error {
	lines, nl, idx, err := editLoad(path)
	if err != nil {
		return err
	}
	if err := checkReadIndex(lines, idx, index, expectTitle); err != nil {
		return err
	}
	lines[idx[index]] = row.render()
	return vault.AtomicWrite(path, []byte(strings.Join(lines, nl)), 0o644)
}

// DeleteReadAt removes the index-th completed-read row. Deleting the first data
// row (which, per the note's convention, doubles as the markdown header) would
// leave the separator on top and break the table render, so the next data row is
// promoted to header in that case. expectTitle guards a stale index as in
// UpdateReadAt.
func DeleteReadAt(path string, index int, expectTitle string) error {
	lines, nl, idx, err := editLoad(path)
	if err != nil {
		return err
	}
	if err := checkReadIndex(lines, idx, index, expectTitle); err != nil {
		return err
	}
	target := idx[index]
	out := make([]string, 0, len(lines)-1)
	out = append(out, lines[:target]...)
	out = append(out, lines[target+1:]...)

	// Header row removed → promote the next data row above the now-orphaned
	// separator so the table keeps a header (writer output is contiguous, so the
	// separator sits immediately after the removed header).
	if index == 0 {
		if s := firstTableLine(out, target); s >= 0 && isSeparatorLine(out[s]) {
			if d := s + 1; d < len(out) && hasWikilinkRow(out[d]) {
				out[s], out[d] = out[d], out[s]
			}
		}
	}
	// If that was the last data row, drop any leftover separator lines too, so the
	// table is fully gone and a later AppendCompleted bootstraps a fresh header
	// rather than inserting under a lone `| --- |`.
	if len(matchedRowIndices(out)) == 0 {
		out = dropSeparators(out)
	}
	return vault.AtomicWrite(path, []byte(strings.Join(out, nl)), 0o644)
}

// editLoad reads the log into lines (newline style preserved) plus the file-line
// indices of every completed-read row, in the same order Parse yields them.
func editLoad(path string) (lines []string, nl string, idx []int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", nil, err
	}
	nl = "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		nl = "\r\n"
	}
	lines = strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	return lines, nl, matchedRowIndices(lines), nil
}

// matchedRowIndices returns the file-line index of every row Parse would keep
// (a `|`-row whose second cell holds a [[wikilink]]), skipping frontmatter — so
// idx[i] is the physical line of Parse's i-th Read.
func matchedRowIndices(lines []string) []int {
	var idx []int
	for i := frontmatterEnd(lines); i < len(lines); i++ {
		if hasWikilinkRow(lines[i]) {
			idx = append(idx, i)
		}
	}
	return idx
}

// frontmatterEnd returns the index of the first body line after a leading
// `---`…`---` YAML block (mirroring stripFrontmatter), or 0 when there's none.
func frontmatterEnd(lines []string) int {
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "---" {
		return 0
	}
	for j := i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "---" {
			return j + 1
		}
	}
	return 0
}

// checkReadIndex bounds-checks index and, when expectTitle is set, verifies the
// row still names it (so a shifted log doesn't get the wrong row edited/deleted).
func checkReadIndex(lines []string, idx []int, index int, expectTitle string) error {
	if index < 0 || index >= len(idx) {
		return fmt.Errorf("reading-log row %d is out of range", index)
	}
	if expectTitle == "" {
		return nil
	}
	cells := splitRow(strings.TrimSpace(lines[idx[index]]))
	m := titleRE.FindStringSubmatch(cells[1])
	if m == nil || strings.TrimSpace(m[1]) != strings.TrimSpace(expectTitle) {
		return fmt.Errorf("reading-log row %d no longer matches %q — refresh and retry", index, expectTitle)
	}
	return nil
}

// hasWikilinkRow reports whether a line is a table row whose second cell carries
// a [[wikilink]] — i.e. a real completed-read row (not the separator or a blank).
func hasWikilinkRow(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") {
		return false
	}
	cells := splitRow(t)
	return len(cells) >= 2 && titleRE.FindStringSubmatch(cells[1]) != nil
}

// firstTableLine returns the index of the first `|`-row at or after from, or -1.
func firstTableLine(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
			return i
		}
	}
	return -1
}

// isSeparatorLine reports whether a line is a markdown table separator row
// (`| --- | ... |`): a `|`-row with dashes and no wikilink.
func isSeparatorLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") && !titleRE.MatchString(t) && strings.Contains(t, "-")
}

// dropSeparators removes any table separator lines — used after the last data
// row is deleted so no orphaned `| --- |` is left to confuse the next append.
func dropSeparators(lines []string) []string {
	out := lines[:0:0]
	for _, l := range lines {
		if isSeparatorLine(l) {
			continue
		}
		out = append(out, l)
	}
	return out
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
