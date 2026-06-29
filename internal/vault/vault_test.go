package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `---
Series: Test Novel
Link: https://example.com/test
Volumes: 2
Read Volumes: 2
Status: reading
Cover: "[[cover.jpg]]"
tags:
  - "#LightNovel"
Template_used: LightNovelTemplate
---
### Test Novel
body text
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A note that can't be parsed (here: a frontmatter line larger than the
// scanner's 1 MiB buffer, the portable stand-in for a locked file / unhydrated
// OneDrive placeholder) must be skipped, not abort the whole scan.
func TestScan_skipsUnparseableNote(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good.md", sample)
	deep := strings.Replace(sample, "https://example.com/test", "https://example.com/two", 1)
	write("good2.md", deep)
	// Single line > 1 MiB → bufio.Scanner returns "token too long" → parse errors.
	write("bad.md", "---\n"+strings.Repeat("x", 2<<20)+"\n---\n")

	entries, err := Scan(dir)
	if err != nil {
		t.Fatalf("one bad note aborted the scan: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected the 2 good notes, got %d: %+v", len(entries), entries)
	}
}

func TestUpdateVolumes_bumpAndInsertLastUpdate(t *testing.T) {
	p := writeTemp(t, sample)

	if err := UpdateVolumes(p, 3, "2026-06-17"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)

	hasVol3, hasVol2 := false, false
	for _, l := range strings.Split(out, "\n") {
		switch strings.TrimRight(l, "\r") {
		case "Volumes: 3":
			hasVol3 = true
		case "Volumes: 2":
			hasVol2 = true
		}
	}
	if !hasVol3 || hasVol2 {
		t.Errorf("Volumes not bumped to 3 (hasVol3=%v hasVol2=%v):\n%s", hasVol3, hasVol2, out)
	}
	if !strings.Contains(out, "Last Update: 2026-06-17") {
		t.Errorf("Last Update not inserted:\n%s", out)
	}
	// Untouched fields.
	for _, must := range []string{"Status: reading", "Read Volumes: 2", "### Test Novel", "body text", `Cover: "[[cover.jpg]]"`} {
		if !strings.Contains(out, must) {
			t.Errorf("clobbered %q:\n%s", must, out)
		}
	}
	// Re-parse confirms the new volume reads back.
	e, ok, err := parse(p)
	if err != nil || !ok || e.Volumes != 3 {
		t.Errorf("reparse: ok=%v vol=%d err=%v", ok, e.Volumes, err)
	}
}

func TestUpdateVolumes_replacesExistingLastUpdate(t *testing.T) {
	p := writeTemp(t, strings.Replace(sample,
		"Read Volumes: 2", "Read Volumes: 2\nLast Update: 2020-01-01", 1))

	if err := UpdateVolumes(p, 5, "2026-06-17"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)

	if strings.Contains(out, "2020-01-01") {
		t.Errorf("old Last Update not replaced:\n%s", out)
	}
	if strings.Count(out, "Last Update:") != 1 {
		t.Errorf("expected exactly one Last Update line:\n%s", out)
	}
}

func TestUpdateVolumes_preservesCRLF(t *testing.T) {
	p := writeTemp(t, strings.ReplaceAll(sample, "\n", "\r\n"))

	if err := UpdateVolumes(p, 4, "2026-06-17"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "\r\n") {
		t.Errorf("CRLF newlines not preserved")
	}
}

func TestUpdateStatus_scalarToList(t *testing.T) {
	// sample has "Status: reading" (scalar) → convert to list format
	p := writeTemp(t, sample)
	if err := UpdateStatus(p, "Queue"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	if !strings.Contains(out, "Status:\n") && !strings.Contains(out, "Status:\r\n") {
		t.Errorf("expected Status: key-only line:\n%s", out)
	}
	if !strings.Contains(out, "  - Queue") {
		t.Errorf("expected '  - Queue':\n%s", out)
	}
	if strings.Contains(out, "Status: reading") {
		t.Errorf("old scalar Status not removed:\n%s", out)
	}
	e, ok, err := parse(p)
	if err != nil || !ok || e.Status != "Queue" {
		t.Errorf("reparse: ok=%v status=%q err=%v", ok, e.Status, err)
	}
}

func TestUpdateStatus_listFormat(t *testing.T) {
	content := strings.Replace(sample, "Status: reading", "Status:\n  - Completed", 1)
	p := writeTemp(t, content)
	if err := UpdateStatus(p, "Queue"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	if !strings.Contains(out, "  - Queue") {
		t.Errorf("expected '  - Queue':\n%s", out)
	}
	if strings.Contains(out, "  - Completed") {
		t.Errorf("old list value not replaced:\n%s", out)
	}
	e, ok, err := parse(p)
	if err != nil || !ok || e.Status != "Queue" {
		t.Errorf("reparse: ok=%v status=%q err=%v", ok, e.Status, err)
	}
}

func TestUpdateStatus_noField(t *testing.T) {
	content := strings.Replace(sample, "Status: reading\n", "", 1)
	p := writeTemp(t, content)
	if err := UpdateStatus(p, "Queue"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	if !strings.Contains(out, "  - Queue") {
		t.Errorf("expected '  - Queue' inserted:\n%s", out)
	}
	e, ok, err := parse(p)
	if err != nil || !ok || e.Status != "Queue" {
		t.Errorf("reparse: ok=%v status=%q err=%v", ok, e.Status, err)
	}
}

func TestUpdateStatus_preservesCRLF(t *testing.T) {
	p := writeTemp(t, strings.ReplaceAll(sample, "\n", "\r\n"))
	if err := UpdateStatus(p, "Queue"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "\r\n") {
		t.Errorf("CRLF newlines not preserved")
	}
}

func TestUpdateStatus_preservesOtherFields(t *testing.T) {
	p := writeTemp(t, sample)
	if err := UpdateStatus(p, "Completed"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	for _, must := range []string{"Volumes: 2", "Read Volumes: 2", `Cover: "[[cover.jpg]]"`, "### Test Novel", "body text"} {
		if !strings.Contains(out, must) {
			t.Errorf("clobbered %q:\n%s", must, out)
		}
	}
}

const bookSample = `---
Title: Rich Dad Poor Dad
Author: Robert T. Kiyosaki
Link: https://openlibrary.org/works/OL20749838W
Work ID: OL20749838W
Cover: "[[cover_RichDadPoorDad.jpg]]"
Released EN: 1997
Status:
  - Queue
tags:
  - "#Book"
Template_used: BookTemplate
created: 2026-06-29
modified: 2026-06-29
---
### Rich Dad Poor Dad
`

func TestScan_picksUpBookNote(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Rich Dad Poor Dad.md"), []byte(bookSample), 0o644); err != nil {
		t.Fatal(err)
	}
	// Also add a regular LN note so we verify both come back.
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (1 LN + 1 book), got %d: %+v", len(entries), entries)
	}

	var book, ln *Entry
	for i := range entries {
		switch entries[i].Kind {
		case "book":
			book = &entries[i]
		case "ln":
			ln = &entries[i]
		}
	}
	if book == nil {
		t.Fatal("no book entry returned")
	}
	if ln == nil {
		t.Fatal("no ln entry returned")
	}

	if book.Title != "Rich Dad Poor Dad" {
		t.Errorf("book title: %q", book.Title)
	}
	if book.Author != "Robert T. Kiyosaki" {
		t.Errorf("book author: %q", book.Author)
	}
	if book.WorkID != "OL20749838W" {
		t.Errorf("book work id: %q", book.WorkID)
	}
	if book.ReleasedEN != "1997" {
		t.Errorf("book released en: %q", book.ReleasedEN)
	}
	if book.Status != "Queue" {
		t.Errorf("book status: %q", book.Status)
	}
	if book.Cover != "cover_RichDadPoorDad.jpg" {
		t.Errorf("book cover: %q", book.Cover)
	}
	if book.Volumes != 0 {
		t.Errorf("book volumes should be zero: %d", book.Volumes)
	}
}

func TestScan_tagAndLinkFilter(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good.md", sample)                                     // tagged + Link → in
	write("nolink.md", "---\ntags:\n  - \"#LightNovel\"\n---\n") // tagged, no Link → out
	write("notag.md", "---\nLink: https://example.com/x\n---\n") // Link, no tag → out
	write("readme.txt", sample)                                  // not .md → ignored
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Folder-agnostic: a tagged note in a subdir is found too.
	deep := strings.Replace(sample, "https://example.com/test", "https://example.com/deep", 1)
	if err := os.WriteFile(filepath.Join(sub, "deep.md"), []byte(deep), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 LN notes, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Cover != "cover.jpg" {
			t.Errorf("cover not parsed: %q", e.Cover)
		}
		if e.Volumes != 2 {
			t.Errorf("volumes not parsed: %d", e.Volumes)
		}
	}
}
