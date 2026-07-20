package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildLNVolumeNote(t *testing.T) {
	got := BuildLNVolumeNote("Kumo Desu ga Nani ka", 7, 16, "eng", "https://jnovels.com/kumo-vol-7-epub/", "cover_KumoVol7.jpg", "2021", "Spider survives.", "2026-07-17", false)
	for _, want := range []string{
		"Title: Kumo Desu ga Nani ka Volume 7",
		`Series: "[[Kumo Desu ga Nani ka]]"`, // linked series property
		"Series Index: 7",
		"Link: https://jnovels.com/kumo-vol-7-epub/",
		"Language: eng",
		"Released EN: 2021",
		`Cover: "[[cover_KumoVol7.jpg]]"`,
		`  - "#LNVolume"`,
		"Status:\n  - Backlog", // new volume notes start Backlog (#102)
		"### Kumo Desu ga Nani ka Volume 7",
		"![[cover_KumoVol7.jpg]]",
		"Spider survives.",
		// Reading navigation: prev/next (both exist for vol 7 of 16).
		"Previous: [[Kumo Desu ga Nani ka Volume 6]]",
		"Next: [[Kumo Desu ga Nani ka Volume 8]]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("note missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "#LNVolume/incomplete") {
		t.Error("a complete note must not carry the incomplete tag")
	}
}

// Nav footer omits Previous on volume 1 and Next on the last volume.
func TestBuildLNVolumeNote_navEdges(t *testing.T) {
	first := BuildLNVolumeNote("S", 1, 3, "", "", "", "", "", "2026-07-17", false)
	if strings.Contains(first, "Previous:") {
		t.Error("volume 1 must not have a Previous link")
	}
	if !strings.Contains(first, "Next: [[S Volume 2]]") {
		t.Error("volume 1 should link Next to volume 2")
	}
	last := BuildLNVolumeNote("S", 3, 3, "", "", "", "", "", "2026-07-17", false)
	if strings.Contains(last, "Next:") {
		t.Error("the last volume must not have a Next link")
	}
	if !strings.Contains(last, "Previous: [[S Volume 2]]") {
		t.Error("the last volume should link Previous to volume 2")
	}
}

func TestBuildLNVolumeNote_incomplete(t *testing.T) {
	got := BuildLNVolumeNote("Overlord", 3, 5, "", "", "", "", "", "2026-07-17", true)
	if !strings.Contains(got, `  - "#LNVolume/incomplete"`) {
		t.Errorf("incomplete note must carry the incomplete tag:\n%s", got)
	}
	if !strings.Contains(got, "Cover:\n") {
		t.Errorf("incomplete note should have a blank Cover field:\n%s", got)
	}
	if strings.Contains(got, "![[") {
		t.Error("incomplete note must not embed a cover")
	}
}

// SetVolumeStatus rewrites an existing volume note's Status, and is a silent
// no-op (nil) when the volume was never backfilled (#102).
func TestSetVolumeStatus(t *testing.T) {
	dir := t.TempDir()
	seriesNotePath := filepath.Join(dir, "S.md") // its VolumeDir is dir/_volumes/S/
	volPath := VolumePath(seriesNotePath, "S", 2)
	if err := os.MkdirAll(filepath.Dir(volPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(volPath, []byte(BuildLNVolumeNote("S", 2, 3, "", "", "", "", "", "2026-07-17", false)), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetVolumeStatus(seriesNotePath, "S", 2, VolumeStatusReading); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(volPath); !strings.Contains(string(data), "Status:\n  - Reading") {
		t.Errorf("Status not flipped to Reading:\n%s", data)
	}

	if err := SetVolumeStatus(seriesNotePath, "S", 2, VolumeStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(volPath); !strings.Contains(string(data), "Status:\n  - Completed") {
		t.Errorf("Status not flipped to Completed:\n%s", data)
	}

	// A volume with no note on disk must not error.
	if err := SetVolumeStatus(seriesNotePath, "S", 3, VolumeStatusReading); err != nil {
		t.Errorf("missing volume note should be a no-op, got %v", err)
	}
}

// SetVolumeNotes writes the ## Notes section of an existing volume note and
// reports written=false (no error) for a volume that was never backfilled (#103).
func TestSetVolumeNotes(t *testing.T) {
	dir := t.TempDir()
	seriesNotePath := filepath.Join(dir, "S.md")
	volPath := VolumePath(seriesNotePath, "S", 2)
	if err := os.MkdirAll(filepath.Dir(volPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(volPath, []byte(BuildLNVolumeNote("S", 2, 3, "", "", "", "", "", "2026-07-17", false)), 0o644); err != nil {
		t.Fatal(err)
	}

	written, err := SetVolumeNotes(seriesNotePath, "S", 2, "Best volume yet.")
	if err != nil || !written {
		t.Fatalf("write to existing note: written=%v err=%v", written, err)
	}
	if got, _ := VolumeNotes(seriesNotePath, "S", 2); got != "Best volume yet." {
		t.Errorf("round-trip mismatch: %q", got)
	}
	// The notes must sit above the prev/next nav footer.
	data, _ := os.ReadFile(volPath)
	if strings.Index(string(data), "## Notes") > strings.Index(string(data), "Next:") {
		t.Errorf("Notes landed below the nav footer:\n%s", data)
	}

	// A volume with no backfilled note: nothing written, no error, empty read.
	if written, err := SetVolumeNotes(seriesNotePath, "S", 3, "orphan note"); err != nil || written {
		t.Errorf("missing volume note should be a no-op: written=%v err=%v", written, err)
	}
	if got, err := VolumeNotes(seriesNotePath, "S", 3); err != nil || got != "" {
		t.Errorf("missing volume note should read empty: got %q err %v", got, err)
	}
}

func TestCreateLNVolume_incompleteAndCollision(t *testing.T) {
	vaultDir := t.TempDir()
	o := Options{VaultDir: vaultDir, NewNoteDir: "LN", AttachmentsDir: "LN/_attachments"}

	res, err := CreateLNVolume(o, "Overlord", 2, 3, "eng", "", "", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(vaultDir, "LN", "_volumes", "Overlord", "Overlord Volume 2.md")
	if res.Path != want {
		t.Errorf("path = %q, want %q", res.Path, want)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "#LNVolume/incomplete") {
		t.Error("written note should be tagged incomplete")
	}
	if res.Cover != "" {
		t.Errorf("incomplete note should have no cover, got %q", res.Cover)
	}

	// A second create for the same volume must refuse rather than clobber.
	if _, err := CreateLNVolume(o, "Overlord", 2, 3, "eng", "", "", "", "", true); err == nil {
		t.Error("expected ErrNoteExists on a repeat create")
	}
}

func TestVolumeDir(t *testing.T) {
	seriesNote := filepath.Join("X", "LN", "Kumo Desu ga Nani ka.md")
	want := filepath.Join("X", "LN", "_volumes", "Kumo Desu ga Nani ka")
	if got := VolumeDir(seriesNote); got != want {
		t.Errorf("VolumeDir = %q, want %q", got, want)
	}
}

func TestAppendVolumeLinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Kumo Desu ga Nani ka.md")
	seriesNote := "---\nSeries: Kumo Desu ga Nani ka\n---\n### Kumo Desu ga Nani ka\n\n![[c.jpg]]\n\n[[Light Novel]]\n\nA spider's tale.\n"
	if err := os.WriteFile(p, []byte(seriesNote), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendVolumeLinks(p, "Kumo Desu ga Nani ka", 3); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	for _, want := range []string{
		"A spider's tale.", // description preserved
		"### Volumes",
		"- [[Kumo Desu ga Nani ka Volume 1]]",
		"- [[Kumo Desu ga Nani ka Volume 3]]",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("series note missing %q\n---\n%s", want, got)
		}
	}
	// The section comes after the description.
	if strings.Index(string(got), "A spider's tale.") > strings.Index(string(got), "### Volumes") {
		t.Error("volume links should follow the description")
	}
	// Idempotent — a second call must not duplicate the section.
	if err := AppendVolumeLinks(p, "Kumo Desu ga Nani ka", 3); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(p)
	if strings.Count(string(again), "### Volumes") != 1 {
		t.Errorf("expected exactly one Volumes section, got %d", strings.Count(string(again), "### Volumes"))
	}
}

func TestVolumeStatesAndRemoveIncomplete(t *testing.T) {
	vaultDir := t.TempDir()
	o := Options{VaultDir: vaultDir, NewNoteDir: "LN", AttachmentsDir: "LN/_attachments"}
	series := "Kumo Desu ga Nani ka"
	seriesNote := filepath.Join(vaultDir, "LN", series+".md")

	// Volume 1 resolved, volume 2 incomplete, volume 3 missing.
	if _, err := CreateLNVolume(o, series, 1, 3, "eng", "https://jnovels.com/k1/", "", "", "Blurb.", false); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateLNVolume(o, series, 2, 3, "eng", "", "", "", "", true); err != nil {
		t.Fatal(err)
	}

	states := VolumeStates(seriesNote, series, 3)
	want := []string{"resolved", "incomplete", "missing"}
	if len(states) != 3 {
		t.Fatalf("got %d states, want 3", len(states))
	}
	for i, w := range want {
		if states[i].State != w {
			t.Errorf("volume %d state = %q, want %q", i+1, states[i].State, w)
		}
	}
	if states[0].Link != "https://jnovels.com/k1/" {
		t.Errorf("resolved volume should carry its link, got %q", states[0].Link)
	}
	// The cover grid + enlarge (#8) need the resolved volume's description; vol 1
	// was created without a cover, so HasCover must be false.
	if states[0].Description != "Blurb." {
		t.Errorf("resolved volume should carry its description (nav footer stripped), got %q", states[0].Description)
	}
	if states[0].HasCover {
		t.Error("resolved volume created without a cover should report HasCover=false")
	}
	if VolumeStateOf(seriesNote, series, 2) != "incomplete" {
		t.Error("VolumeStateOf should report volume 2 incomplete")
	}

	// RemoveIncompleteVolumes drops only the incomplete note.
	removed, err := RemoveIncompleteVolumes(seriesNote)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}
	if VolumeStateOf(seriesNote, series, 1) != "resolved" {
		t.Error("the resolved volume must survive RemoveIncompleteVolumes")
	}
	if VolumeStateOf(seriesNote, series, 2) != "missing" {
		t.Error("the incomplete volume should now be gone")
	}
}

// The reading-view cover lookup derives the _volumes dir from the series note's
// path; CreateLNVolume must write into that very dir — including for a title with
// a (curly) apostrophe, where any stray normalization would split them apart and
// silently lose the volume cover.
func TestCreateLNVolume_dirMatchesVolumeDir(t *testing.T) {
	vaultDir := t.TempDir()
	o := Options{VaultDir: vaultDir, NewNoteDir: "LN", AttachmentsDir: "LN/_attachments"}
	series := "Marielle Clarac’s Musings" // curly apostrophe, as a scraped title carries

	// The series note lives at <NewNoteDir>/<Sanitize(title)>.md (how notes.Create names it).
	seriesNote := filepath.Join(vaultDir, "LN", Sanitize(series, false)+".md")

	res, err := CreateLNVolume(o, series, 1, 1, "eng", "", "", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Dir(res.Path), VolumeDir(seriesNote); got != want {
		t.Errorf("volume note dir = %q, want the reading-view lookup dir %q", got, want)
	}
}
