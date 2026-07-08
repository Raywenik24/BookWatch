package importer

import (
	"os"
	"path/filepath"
	"testing"

	"bookwatch/internal/notes"
)

// stageFixture stages one matched LN series (two volumes, one with a cover) and
// one regular #Book (with a cover) under a fresh staging dir, returning it.
func stageFixture(t *testing.T) string {
	t.Helper()
	staging := t.TempDir()
	src := t.TempDir()
	seriesCover := writeCover(t, filepath.Join(src, "series.jpg"))
	bookCover := writeCover(t, filepath.Join(src, "book.jpg"))

	w := NewWriter(staging, testToday)
	if _, err := w.StageLNSeries(PlanLNSeries{
		Series:    "Overlord",
		Link:      "https://jnovels.com/overlord/",
		CoverPath: seriesCover,
		Volumes: []PlanVolume{
			{Title: "Overlord Vol 1", SeriesIndex: 1, Done: true},
			{Title: "Overlord Vol 2", SeriesIndex: 2, CoverPath: writeCover(t, filepath.Join(src, "v2.jpg"))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.StageBook(PlanBook{
		Title:     "The Institute",
		Link:      "https://openlibrary.org/works/OL1W",
		CoverPath: bookCover,
	}); err != nil {
		t.Fatal(err)
	}
	return staging
}

func destDirs(t *testing.T) (root string, d FinalizeDest) {
	t.Helper()
	root = t.TempDir()
	return root, FinalizeDest{
		NoteDirLN:     filepath.Join(root, "01_LightNovel_db"),
		AttachDirLN:   filepath.Join(root, "01_LightNovel_db", "_attachments"),
		NoteDirBook:   filepath.Join(root, "02_Books_db"),
		AttachDirBook: filepath.Join(root, "02_Books_db", "_attachments"),
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(longPath(path))
	return err == nil
}

func TestFinalizeMovesNotesAndCovers(t *testing.T) {
	staging := stageFixture(t)
	_, d := destDirs(t)

	res, err := Finalize(staging, d)
	if err != nil {
		t.Fatal(err)
	}
	// 1 series + 2 volumes + 1 book = 4 notes; series + v2 + book covers = 3.
	if res.Notes != 4 || res.Covers != 3 {
		t.Fatalf("moved = %d notes / %d covers, want 4 / 3", res.Notes, res.Covers)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("unexpected skips: %v", res.Skipped)
	}

	// Series note → LN dir; volumes → LN/_volumes/<Series>/; book → Book dir.
	for _, p := range []string{
		filepath.Join(d.NoteDirLN, "Overlord.md"),
		filepath.Join(d.NoteDirLN, "_volumes", "Overlord", "Overlord Vol 1.md"),
		filepath.Join(d.NoteDirLN, "_volumes", "Overlord", "Overlord Vol 2.md"),
		filepath.Join(d.NoteDirBook, "The Institute.md"),
		filepath.Join(d.AttachDirLN, "cover_Overlord.jpg"),
		filepath.Join(d.AttachDirLN, notes.CoverName("Overlord Vol 2", ".jpg")),
		filepath.Join(d.AttachDirBook, "cover_TheInstitute.jpg"),
	} {
		if !exists(t, p) {
			t.Errorf("expected moved to %s", p)
		}
	}
	if exists(t, filepath.Join(staging, "Overlord.md")) {
		t.Error("series note should have left staging")
	}
}

func TestFinalizeCollisionSafe(t *testing.T) {
	staging := stageFixture(t)
	_, d := destDirs(t)

	// Pre-create the book note's target so finalize must not clobber it.
	if err := os.MkdirAll(longPath(d.NoteDirBook), 0o755); err != nil {
		t.Fatal(err)
	}
	guard := filepath.Join(d.NoteDirBook, "The Institute.md")
	if err := os.WriteFile(longPath(guard), []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Finalize(staging, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "The Institute.md" {
		t.Fatalf("skipped = %v, want [The Institute.md]", res.Skipped)
	}
	// The pre-existing note is untouched, and the skipped note stays in staging.
	if b, _ := os.ReadFile(longPath(guard)); string(b) != "ORIGINAL" {
		t.Errorf("collision clobbered existing note: %q", b)
	}
	if !exists(t, filepath.Join(staging, "The Institute.md")) {
		t.Error("skipped note should remain in staging")
	}
	// Its cover must NOT have moved (only referenced covers of moved notes travel).
	if exists(t, filepath.Join(d.AttachDirBook, "cover_TheInstitute.jpg")) {
		t.Error("skipped note's cover should stay in staging")
	}
}

func TestFinalizeLeavesReportAndDeletedNoteCover(t *testing.T) {
	staging := stageFixture(t)
	_, d := destDirs(t)

	// Simulate the reviewer deleting the book note in Obsidian (its cover stays).
	if err := os.Remove(longPath(filepath.Join(staging, "The Institute.md"))); err != nil {
		t.Fatal(err)
	}
	// Drop a report so we can assert it isn't moved or treated as a note.
	if _, _, err := WriteReport(staging, testToday, nil); err != nil {
		t.Fatal(err)
	}

	res, err := Finalize(staging, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Notes != 3 { // series + 2 volumes; the deleted book is gone
		t.Fatalf("moved %d notes, want 3", res.Notes)
	}
	// The orphaned cover of the deleted note stays behind.
	if !exists(t, filepath.Join(staging, "cover_TheInstitute.jpg")) {
		t.Error("orphan cover of a deleted note should be left in staging")
	}
	// The report is never migrated.
	if exists(t, filepath.Join(d.NoteDirBook, reportName)) || exists(t, filepath.Join(d.NoteDirLN, reportName)) {
		t.Error("report should not be moved")
	}
	if !exists(t, filepath.Join(staging, reportName)) {
		t.Error("report should stay in staging")
	}
}
