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
