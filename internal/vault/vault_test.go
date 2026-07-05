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
	if err := UpdateStatus(p, "Backlog"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	if !strings.Contains(out, "Status:\n") && !strings.Contains(out, "Status:\r\n") {
		t.Errorf("expected Status: key-only line:\n%s", out)
	}
	if !strings.Contains(out, "  - Backlog") {
		t.Errorf("expected '  - Backlog':\n%s", out)
	}
	if strings.Contains(out, "Status: reading") {
		t.Errorf("old scalar Status not removed:\n%s", out)
	}
	e, ok, err := parse(p)
	if err != nil || !ok || e.Status != "Backlog" {
		t.Errorf("reparse: ok=%v status=%q err=%v", ok, e.Status, err)
	}
}

func TestUpdateStatus_listFormat(t *testing.T) {
	content := strings.Replace(sample, "Status: reading", "Status:\n  - Completed", 1)
	p := writeTemp(t, content)
	if err := UpdateStatus(p, "Backlog"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	if !strings.Contains(out, "  - Backlog") {
		t.Errorf("expected '  - Backlog':\n%s", out)
	}
	if strings.Contains(out, "  - Completed") {
		t.Errorf("old list value not replaced:\n%s", out)
	}
	e, ok, err := parse(p)
	if err != nil || !ok || e.Status != "Backlog" {
		t.Errorf("reparse: ok=%v status=%q err=%v", ok, e.Status, err)
	}
}

func TestUpdateStatus_noField(t *testing.T) {
	content := strings.Replace(sample, "Status: reading\n", "", 1)
	p := writeTemp(t, content)
	if err := UpdateStatus(p, "Backlog"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	if !strings.Contains(out, "  - Backlog") {
		t.Errorf("expected '  - Backlog' inserted:\n%s", out)
	}
	e, ok, err := parse(p)
	if err != nil || !ok || e.Status != "Backlog" {
		t.Errorf("reparse: ok=%v status=%q err=%v", ok, e.Status, err)
	}
}

func TestUpdateStatus_preservesCRLF(t *testing.T) {
	p := writeTemp(t, strings.ReplaceAll(sample, "\n", "\r\n"))
	if err := UpdateStatus(p, "Backlog"); err != nil {
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
  - Backlog
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
	if book.Status != "Backlog" {
		t.Errorf("book status: %q", book.Status)
	}
	if book.Cover != "cover_RichDadPoorDad.jpg" {
		t.Errorf("book cover: %q", book.Cover)
	}
	if book.Volumes != 0 {
		t.Errorf("book volumes should be zero: %d", book.Volumes)
	}
}

func TestReadNote_lnAndBook(t *testing.T) {
	// LN: description body + volumes + tags.
	p := writeTemp(t, sample)
	n, err := ReadNote(p)
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "ln" {
		t.Errorf("kind: %q", n.Kind)
	}
	if n.Volumes != 2 || !n.HasReadVolumes || n.ReadVolumes != 2 {
		t.Errorf("volumes=%d read=%d has=%v", n.Volumes, n.ReadVolumes, n.HasReadVolumes)
	}
	if n.Description != "body text" {
		t.Errorf("description: %q", n.Description)
	}
	if len(n.Tags) != 1 || n.Tags[0] != "LightNovel" {
		t.Errorf("tags: %+v", n.Tags)
	}

	// Book: author, work id, released, status list, cover, tags — no description.
	bp := writeTemp(t, bookSample)
	b, err := ReadNote(bp)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != "book" {
		t.Errorf("kind: %q", b.Kind)
	}
	if b.Author != "Robert T. Kiyosaki" || b.WorkID != "OL20749838W" || b.ReleasedEN != "1997" {
		t.Errorf("book fields: %+v", b)
	}
	if b.Status != "Backlog" {
		t.Errorf("status: %q", b.Status)
	}
	if b.Cover != "cover_RichDadPoorDad.jpg" {
		t.Errorf("cover: %q", b.Cover)
	}
	if len(b.Tags) != 1 || b.Tags[0] != "Book" {
		t.Errorf("tags: %+v", b.Tags)
	}
	if b.Description != "" {
		t.Errorf("expected empty description, got %q", b.Description)
	}
}

func TestUpdateReleasedEN(t *testing.T) {
	p := writeTemp(t, bookSample)
	if err := UpdateReleasedEN(p, "2001"); err != nil {
		t.Fatal(err)
	}
	n, _ := ReadNote(p)
	if n.ReleasedEN != "2001" {
		t.Errorf("released en: %q", n.ReleasedEN)
	}
	// Inserts when absent.
	p2 := writeTemp(t, sample)
	if err := UpdateReleasedEN(p2, "1999"); err != nil {
		t.Fatal(err)
	}
	n2, _ := ReadNote(p2)
	if n2.ReleasedEN != "1999" {
		t.Errorf("released en inserted: %q", n2.ReleasedEN)
	}
}

func TestUpdateTags(t *testing.T) {
	p := writeTemp(t, bookSample)
	if err := UpdateTags(p, []string{"Book", "Finance", "#Favorite"}); err != nil {
		t.Fatal(err)
	}
	n, _ := ReadNote(p)
	if len(n.Tags) != 3 || n.Tags[0] != "Book" || n.Tags[1] != "Finance" || n.Tags[2] != "Favorite" {
		t.Errorf("tags: %+v", n.Tags)
	}
	// Untouched neighbours.
	got, _ := os.ReadFile(p)
	for _, must := range []string{"Template_used: BookTemplate", `Cover: "[[cover_RichDadPoorDad.jpg]]"`, "### Rich Dad Poor Dad"} {
		if !strings.Contains(string(got), must) {
			t.Errorf("clobbered %q:\n%s", must, got)
		}
	}
}

func TestUpdateDescription_preservesStructure(t *testing.T) {
	// LN body has a title heading, cover embed, and the [[Light Novel]] link.
	content := strings.Replace(sample, "### Test Novel\nbody text\n",
		"### Test Novel\n\n![[cover.jpg]]\n\n[[Light Novel]]\n\nold blurb\n", 1)
	p := writeTemp(t, content)
	if err := UpdateDescription(p, "new blurb line 1\nnew blurb line 2"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	for _, must := range []string{"### Test Novel", "![[cover.jpg]]", "[[Light Novel]]", "new blurb line 1", "new blurb line 2"} {
		if !strings.Contains(out, must) {
			t.Errorf("missing %q:\n%s", must, out)
		}
	}
	if strings.Contains(out, "old blurb") {
		t.Errorf("old description not replaced:\n%s", out)
	}
	n, _ := ReadNote(p)
	if n.Description != "new blurb line 1\nnew blurb line 2" {
		t.Errorf("description round-trip: %q", n.Description)
	}
}

func TestUpdateCover_fieldAndEmbed(t *testing.T) {
	// Replace an existing embed + field.
	content := strings.Replace(sample, "### Test Novel\nbody text\n",
		"### Test Novel\n\n![[cover.jpg]]\n\nblurb\n", 1)
	p := writeTemp(t, content)
	if err := UpdateCover(p, "cover_new.png"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), `Cover: "[[cover_new.png]]"`) {
		t.Errorf("cover field not updated:\n%s", out)
	}
	if !strings.Contains(string(out), "![[cover_new.png]]") || strings.Contains(string(out), "![[cover.jpg]]") {
		t.Errorf("embed not updated:\n%s", out)
	}
	n, _ := ReadNote(p)
	if n.Cover != "cover_new.png" {
		t.Errorf("cover: %q", n.Cover)
	}
}

func TestUpdateCover_insertsEmbedWhenMissing(t *testing.T) {
	p := writeTemp(t, bookSample) // body has only the H3 title, no embed
	if err := UpdateCover(p, "cover_x.jpg"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), "![[cover_x.jpg]]") {
		t.Errorf("embed not inserted:\n%s", out)
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
