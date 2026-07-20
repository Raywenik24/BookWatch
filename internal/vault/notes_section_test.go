package vault

import (
	"os"
	"strings"
	"testing"
)

// A #LNVolume note body: H3 title, cover embed, description, then a prev/next
// nav footer — the Notes section must land between the description and the nav.
const volumeNote = `---
Title: Series Volume 3
Series: "[[Series]]"
Series Index: 3
tags:
  - "#LNVolume"
Status:
  - Reading
---
### Series Volume 3

![[Series Volume 3.jpg]]

A gripping third installment.

---
Previous: [[Series Volume 2]]
Next: [[Series Volume 4]]
`

// A #Book note body has no nav footer, so Notes appends at the end.
const bookNote = `---
Title: A Book
Link: https://example.com
tags:
  - "#Book"
Template_used: BookTemplate
---
### A Book

![[A Book.jpg]]

The blurb.
`

func TestSetNotesSection_VolumeInsertsAboveNav(t *testing.T) {
	p := writeTemp(t, volumeNote)
	if err := SetNotesSection(p, "Loved the finale.\nSecond line."); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)

	if !strings.Contains(got, "## Notes\n\nLoved the finale.\nSecond line.") {
		t.Errorf("notes block not written as expected:\n%s", got)
	}
	// The Notes section must sit before the nav footer, not after it.
	if strings.Index(got, "## Notes") > strings.Index(got, "Previous:") {
		t.Errorf("Notes section landed below the nav footer:\n%s", got)
	}
	// Nav footer + description survive intact.
	for _, want := range []string{"A gripping third installment.", "Previous: [[Series Volume 2]]", "Next: [[Series Volume 4]]"} {
		if !strings.Contains(got, want) {
			t.Errorf("clobbered %q:\n%s", want, got)
		}
	}
	if back, _ := NotesSection(p); back != "Loved the finale.\nSecond line." {
		t.Errorf("round-trip mismatch: %q", back)
	}
}

func TestSetNotesSection_ReplaceIsIdempotent(t *testing.T) {
	p := writeTemp(t, volumeNote)
	if err := SetNotesSection(p, "first"); err != nil {
		t.Fatal(err)
	}
	if err := SetNotesSection(p, "second revised"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if strings.Count(got, "## Notes") != 1 {
		t.Errorf("expected exactly one Notes heading, got:\n%s", got)
	}
	if strings.Contains(got, "first") {
		t.Errorf("old notes not replaced:\n%s", got)
	}
	if back, _ := NotesSection(p); back != "second revised" {
		t.Errorf("want %q, got %q", "second revised", back)
	}
}

func TestSetNotesSection_BlankRemovesSection(t *testing.T) {
	p := writeTemp(t, volumeNote)
	if err := SetNotesSection(p, "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := SetNotesSection(p, "  "); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if strings.Contains(got, "## Notes") {
		t.Errorf("Notes section not removed:\n%s", got)
	}
	// Removing notes must leave the description and nav footer untouched.
	if !strings.Contains(got, "A gripping third installment.") || !strings.Contains(got, "Previous: [[Series Volume 2]]") {
		t.Errorf("removal damaged the body:\n%s", got)
	}
}

func TestSetNotesSection_BookAppendsAtEnd(t *testing.T) {
	p := writeTemp(t, bookNote)
	if err := SetNotesSection(p, "A quiet gem."); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if strings.Index(got, "The blurb.") > strings.Index(got, "## Notes") {
		t.Errorf("Notes should follow the blurb:\n%s", got)
	}
	if back, _ := NotesSection(p); back != "A quiet gem." {
		t.Errorf("round-trip mismatch: %q", back)
	}
}

func TestNotesSection_AbsentIsEmpty(t *testing.T) {
	p := writeTemp(t, volumeNote)
	if got, err := NotesSection(p); err != nil || got != "" {
		t.Errorf("want empty/no-error, got %q / %v", got, err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
