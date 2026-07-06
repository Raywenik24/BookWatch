package reading

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleLog mirrors the real `_Read.md`'s quirks: frontmatter + prose above the
// table, the first data row doubling as the markdown header, a `----` end cell,
// heavy space-padding, a blank volume cell (an in-progress #Book-style row), a
// curly apostrophe in a title, and trailing all-blank spare rows.
const sampleLog = `---
modified: 2026-07-06
---
Currently read [[Light Novel]]s

| 202509 | [[Hell Mode]]                         |  10 |            |            |
| ------ | ------------------------------------- | --: | ---------- | ---------- |
| 202509 | [[Hell Mode]]                         |  10 |            |            |
| 202601 | [[Welcome to Olivia’s Magic Jewelers]] |   1 | 2026.01.02 | 2026.01.05 |
| 202606 | [[The Canon Fodder]]                  |   1 | 2026.06.15 | ----       |
| 202607 | [[Some Book]]                         |     | 2026.07.01 | 2026.07.02 |
|        |                                       |     |            |            |
|        |                                       |     |            |            |
`

func TestParse(t *testing.T) {
	reads := Parse([]byte(sampleLog))
	if len(reads) != 5 {
		t.Fatalf("got %d reads, want 5: %+v", len(reads), reads)
	}

	// First data row (doubles as the header) is parsed as a real read.
	if reads[0] != (Read{YearMonth: "202509", Title: "Hell Mode", Volume: "10"}) {
		t.Errorf("reads[0] = %+v", reads[0])
	}
	// Curly punctuation survives; full dates parsed.
	if reads[2].Title != "Welcome to Olivia’s Magic Jewelers" || reads[2].Start != "2026.01.02" || reads[2].End != "2026.01.05" {
		t.Errorf("reads[2] = %+v", reads[2])
	}
	// `----` end cell collapses to blank.
	if reads[3].End != "" {
		t.Errorf("reads[3].End = %q, want blank (from ----)", reads[3].End)
	}
	// Blank volume cell (whole-work / in-progress) stays blank, not dropped.
	if reads[4].Volume != "" || reads[4].Title != "Some Book" {
		t.Errorf("reads[4] = %+v", reads[4])
	}
}

func TestParseSkipsSeparatorAndBlankRows(t *testing.T) {
	for _, r := range Parse([]byte(sampleLog)) {
		if r.Title == "" {
			t.Fatalf("parsed a titleless row: %+v", r)
		}
		if strings.Contains(r.Title, "--") {
			t.Fatalf("separator row leaked in as a read: %+v", r)
		}
	}
}

func TestParseNoFrontmatterOrTable(t *testing.T) {
	if got := Parse([]byte("just some notes\nno table here\n")); got != nil {
		t.Errorf("expected no reads, got %+v", got)
	}
}

func TestReReadCounts(t *testing.T) {
	reads := Parse([]byte(sampleLog))

	// Hell Mode vol 10 appears twice → a re-read.
	if n := CountFor(reads, "Hell Mode", "10"); n != 2 {
		t.Errorf("Hell Mode ×10 count = %d, want 2", n)
	}
	// A single read is count 1 (badge suppressed by the UI, but the value is 1).
	if n := CountFor(reads, "The Canon Fodder", "1"); n != 1 {
		t.Errorf("Canon Fodder count = %d, want 1", n)
	}
	// #Book (blank volume) counts per title.
	if n := CountFor(reads, "Some Book", ""); n != 1 {
		t.Errorf("Some Book count = %d, want 1", n)
	}
	// A never-read unit is 0.
	if n := CountFor(reads, "Nonexistent", "1"); n != 0 {
		t.Errorf("unknown title count = %d, want 0", n)
	}
}

func TestNewCompletedRowKnownDates(t *testing.T) {
	// Picker dates (YYYY-MM-DD) → log form (YYYY.MM.DD), YYYYMM from the end date.
	row := NewCompletedRow("Hell Mode", "12", "2026-07-01", "2026-07-06", false)
	want := CompletedRow{YearMonth: "202607", Title: "Hell Mode", Volume: "12", Start: "2026.07.01", End: "2026.07.06"}
	if row != want {
		t.Errorf("row = %+v, want %+v", row, want)
	}
}

func TestNewCompletedRowUnknownDates(t *testing.T) {
	// "I don't remember" → blank date cells AND blank YYYYMM; read still logged.
	row := NewCompletedRow("Hell Mode", "12", "2026-07-01", "2026-07-06", true)
	if row.YearMonth != "" || row.Start != "" || row.End != "" {
		t.Errorf("unknown row should have blank dates + YYYYMM, got %+v", row)
	}
	if row.Title != "Hell Mode" || row.Volume != "12" {
		t.Errorf("unknown row lost title/volume: %+v", row)
	}
}

func TestNewCompletedRowYearMonthFromStartFallback(t *testing.T) {
	// Only a start date known → YYYYMM falls back to it.
	row := NewCompletedRow("Some Book", "", "2026-05-13", "", false)
	if row.YearMonth != "202605" {
		t.Errorf("YearMonth = %q, want 202605", row.YearMonth)
	}
}

func TestAppendCompletedLeavesExistingBytesUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_Read.md")
	if err := os.WriteFile(path, []byte(sampleLog), 0o644); err != nil {
		t.Fatal(err)
	}

	before := lastPopulatedRow(sampleLog)
	row := NewCompletedRow("New Title", "3", "2026-07-05", "2026-07-06", false)
	if err := AppendCompleted(path, row); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)

	// Every original line still present verbatim.
	for _, line := range strings.Split(strings.TrimRight(sampleLog, "\n"), "\n") {
		if !strings.Contains(out, line) {
			t.Fatalf("original line dropped/altered: %q", line)
		}
	}

	// The compact row was inserted right after the last populated row and before
	// the trailing blank spare rows.
	newLine := "| 202607 | [[New Title]] | 3 | 2026.07.05 | 2026.07.06 |"
	if !strings.Contains(out, newLine) {
		t.Fatalf("compact row not written; got:\n%s", out)
	}
	idxNew := strings.Index(out, newLine)
	idxAfterLast := strings.Index(out, before) + len(before)
	if idxNew < idxAfterLast {
		t.Errorf("new row inserted before the last populated row")
	}

	// It parses back as a re-readable row.
	reads := Parse(got)
	if CountFor(reads, "New Title", "3") != 1 {
		t.Errorf("appended row did not parse back: %+v", reads)
	}
}

func TestAppendCompletedUnknownDatesBlankCells(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_Read.md")
	if err := os.WriteFile(path, []byte(sampleLog), 0o644); err != nil {
		t.Fatal(err)
	}
	row := NewCompletedRow("Undated Read", "1", "", "", true)
	if err := AppendCompleted(path, row); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "|  | [[Undated Read]] | 1 |  |  |") {
		t.Fatalf("unknown-date row not blank as expected:\n%s", got)
	}
}

func TestAppendCompletedBootstrapsEmptyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_Read.md")
	if err := os.WriteFile(path, []byte("---\nmodified: 2026-07-06\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendCompleted(path, NewCompletedRow("First Book", "", "2026-07-06", "2026-07-06", false)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	if !strings.Contains(out, separatorRow) {
		t.Errorf("bootstrap did not add a separator row:\n%s", out)
	}
	// A second append lands below the separator, not between header and separator.
	if err := AppendCompleted(path, NewCompletedRow("Second Book", "", "2026-07-07", "2026-07-07", false)); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	reads := Parse(got)
	if len(reads) != 2 {
		t.Fatalf("got %d reads after two bootstrapped appends, want 2:\n%s", len(reads), got)
	}
}

// lastPopulatedRow returns the last non-blank table row of a log body — the row
// the next append should land after.
func lastPopulatedRow(body string) string {
	var last string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "|") && !isBlankRow(t) {
			last = t
		}
	}
	return last
}
