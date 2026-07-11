package importer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bookwatch/internal/calibre"
	"bookwatch/internal/provider"
	"bookwatch/internal/scraper"
	"bookwatch/internal/store"
)

var errFake = errors.New("network down")

// newTestStore opens a fresh temp SQLite store.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// testMatcher builds a matcher whose backends resolve a fixed fixture set.
func testMatcher() *Matcher {
	ol := &stubOL{
		byISBN: map[string]provider.Candidate{
			"111": {WorkID: "OL1W", OLURL: "https://openlibrary.org/works/OL1W", Title: "Dune", Author: "Frank Herbert"},
		},
	}
	ln := &stubLN{byQuery: map[string][]scraper.SearchResult{
		"Overlord": {{Title: "Overlord", URL: "https://jnovels.com/overlord/"}},
	}}
	lc := &stubLC{byTitle: map[string][]provider.LCSearchResult{
		"Wiedźmin": {{Title: "Wiedźmin", URL: "https://lubimyczytac.pl/ksiazka/1", Author: "Andrzej Sapkowski", BookID: "1"}},
	}}
	return New(ol, lc, ln, 0)
}

func lnVol(series string, idx float64, uuid string) calibre.Book {
	return calibre.Book{UUID: uuid, Title: series + " Vol", SeriesIndex: idx, Series: series,
		Tags: []string{"Light Novel"}, Languages: []string{"eng"}}
}

func engBook(title, uuid, isbn string) calibre.Book {
	return calibre.Book{UUID: uuid, Title: title, Authors: []string{"Frank Herbert"},
		Languages: []string{"eng"}, Identifiers: map[string]string{"isbn": isbn}}
}

// fullImport wires an Import over a temp store + temp staging dir.
func fullImport(t *testing.T, st *store.Store) (*Import, string) {
	t.Helper()
	stagingDir := t.TempDir()
	return &Import{
		Store:   st,
		Matcher: testMatcher(),
		Writer:  NewWriter(stagingDir, testToday),
		Today:   testToday,
		ScrapeSeries: func(link string) (int, string) {
			return 16, "scraped desc"
		},
	}, stagingDir
}

func TestGroupWorkUnits(t *testing.T) {
	books := []calibre.Book{
		engBook("Dune", "u-dune", "111"),
		lnVol("Overlord", 2, "u-ov2"),
		lnVol("Overlord", 1, "u-ov1"),
		{UUID: "u-standalone", Title: "Solo LN", Tags: []string{"Light Novel"}, Languages: []string{"eng"}},
	}
	units := GroupWorkUnits(books, nil)
	if len(units) != 3 {
		t.Fatalf("want 3 units, got %d", len(units))
	}
	// The Overlord series should carry both volumes, ordered by index.
	var ov *workUnit
	for i := range units {
		if units[i].series == "Overlord" {
			ov = &units[i]
		}
	}
	if ov == nil || len(ov.books) != 2 {
		t.Fatalf("Overlord unit missing or wrong volume count: %+v", ov)
	}
	// Volumes are sorted by index, so books[0] (and thus the unit key) is the
	// lowest-index volume — stable regardless of input order.
	if ov.books[0].SeriesIndex != 1 || ov.key != "u-ov1" {
		t.Errorf("Overlord key/order wrong: key=%s idx0=%v", ov.key, ov.books[0].SeriesIndex)
	}
}

func TestRunStagesAndRecords(t *testing.T) {
	st := newTestStore(t)
	im, stagingDir := fullImport(t, st)

	books := []calibre.Book{
		engBook("Dune", "u-dune", "111"),
		lnVol("Overlord", 1, "u-ov1"),
		lnVol("Overlord", 2, "u-ov2"),
	}
	sid, err := st.CreateImportSession("lib", stagingDir)
	if err != nil {
		t.Fatal(err)
	}
	units := GroupWorkUnits(books, nil)
	if err := im.Run(sid, units); err != nil {
		t.Fatal(err)
	}

	// Session done, both items matched.
	sess, _, _ := st.GetImportSession(sid)
	if sess.Status != "done" || sess.Total != 2 {
		t.Fatalf("session not done/total: %+v", sess)
	}
	items, _ := st.ListImportItems(sid)
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	for _, it := range items {
		if it.State != "matched" {
			t.Errorf("%s state = %s, want matched", it.Title, it.State)
		}
	}
	// The Dune note and the Overlord series note exist on disk.
	if _, err := os.Stat(filepath.Join(stagingDir, "Dune.md")); err != nil {
		t.Errorf("Dune note missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "Overlord.md")); err != nil {
		t.Errorf("Overlord note missing: %v", err)
	}
	// Processed uuids recorded for both units.
	proc, _ := st.ProcessedUUIDs()
	for _, u := range []string{"u-dune", "u-ov1", "u-ov2"} {
		if !proc[u] {
			t.Errorf("uuid %s not marked processed", u)
		}
	}
}

func TestBookDescriptionFallback(t *testing.T) {
	st := newTestStore(t)
	im, stagingDir := fullImport(t, st)
	calls := 0
	im.ScrapeBookDescription = func(kind Kind, link, workID string) string {
		calls++
		return "source blurb"
	}

	// One matched book with no Calibre comments (→ fallback fills it) and one with
	// comments (→ kept, fallback not called). Both resolve via the same stub ISBN.
	withComments := engBook("Dune Two", "u-dune2", "111")
	withComments.Comments = "calibre blurb"

	sid, err := st.CreateImportSession("lib", stagingDir)
	if err != nil {
		t.Fatal(err)
	}
	units := GroupWorkUnits([]calibre.Book{engBook("Dune", "u-dune", "111"), withComments}, nil)
	if err := im.Run(sid, units); err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Fatalf("fallback called %d times, want 1 (only the comment-less book)", calls)
	}
	if dune := readFile(t, filepath.Join(stagingDir, "Dune.md")); !strings.Contains(dune, "source blurb") {
		t.Errorf("Dune note missing source-fallback description:\n%s", dune)
	}
	two := readFile(t, filepath.Join(stagingDir, "Dune Two.md"))
	if !strings.Contains(two, "calibre blurb") || strings.Contains(two, "source blurb") {
		t.Errorf("Dune Two should keep its Calibre comments, not the fallback:\n%s", two)
	}
}

func TestReRunSkipsProcessed(t *testing.T) {
	st := newTestStore(t)
	im, stagingDir := fullImport(t, st)
	books := []calibre.Book{engBook("Dune", "u-dune", "111")}

	sid, _ := st.CreateImportSession("lib", stagingDir)
	if err := im.Run(sid, GroupWorkUnits(books, nil)); err != nil {
		t.Fatal(err)
	}

	// Second run: same book plus a new one — only the new one is enumerated.
	proc, _ := st.ProcessedUUIDs()
	books2 := append(books, engBook("Dune Messiah", "u-dune2", ""))
	remaining := GroupWorkUnits(books2, proc)
	if len(remaining) != 1 || remaining[0].title != "Dune Messiah" {
		t.Fatalf("re-run should surface only the new book, got %+v", remaining)
	}
}

func TestStopThenResume(t *testing.T) {
	st := newTestStore(t)
	im, stagingDir := fullImport(t, st)
	books := []calibre.Book{
		engBook("Dune", "u-dune", "111"),
		engBook("Dune Messiah", "u-dune2", ""),
	}
	sid, _ := st.CreateImportSession("lib", stagingDir)

	// Request stop up front: Run halts immediately, session stays running.
	if err := st.RequestImportStop(sid); err != nil {
		t.Fatal(err)
	}
	if err := im.Run(sid, GroupWorkUnits(books, nil)); err != nil {
		t.Fatal(err)
	}
	sess, _, _ := st.GetImportSession(sid)
	if sess.Status != "running" {
		t.Fatalf("stopped session should stay running, got %s", sess.Status)
	}

	// Resume: clear stop, re-enumerate (processed set is still empty), finish.
	st.ClearImportStop(sid)
	proc, _ := st.ProcessedUUIDs()
	if err := im.Run(sid, GroupWorkUnits(books, proc)); err != nil {
		t.Fatal(err)
	}
	sess, _, _ = st.GetImportSession(sid)
	if sess.Status != "done" {
		t.Fatalf("resumed session should finish done, got %s", sess.Status)
	}
	items, _ := st.ListImportItems(sid)
	if len(items) != 2 {
		t.Fatalf("want 2 items after resume, got %d", len(items))
	}
}

func TestErroredItemRetried(t *testing.T) {
	st := newTestStore(t)
	stagingDir := t.TempDir()
	// A matcher whose LN backend errors, so the LN series item goes errored.
	im := &Import{
		Store:   st,
		Matcher: New(&stubOL{}, nil, &stubLN{err: errFake}, 0),
		Writer:  NewWriter(stagingDir, testToday),
		Today:   testToday,
	}
	books := []calibre.Book{lnVol("Overlord", 1, "u-ov1")}
	sid, _ := st.CreateImportSession("lib", stagingDir)
	if err := im.Run(sid, GroupWorkUnits(books, nil)); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListImportItems(sid)
	if len(items) != 1 || items[0].State != "errored" {
		t.Fatalf("want 1 errored item, got %+v", items)
	}
	// Errored uuids are NOT processed, so a resume re-enumerates them.
	proc, _ := st.ProcessedUUIDs()
	if proc["u-ov1"] {
		t.Error("errored uuid should not be processed")
	}
}

func TestStartOverRemovesFiles(t *testing.T) {
	st := newTestStore(t)
	im, stagingDir := fullImport(t, st)
	books := []calibre.Book{engBook("Dune", "u-dune", "111")}
	sid, _ := st.CreateImportSession("lib", stagingDir)
	if err := im.Run(sid, GroupWorkUnits(books, nil)); err != nil {
		t.Fatal(err)
	}
	notePath := filepath.Join(stagingDir, "Dune.md")
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("note should exist before start-over: %v", err)
	}

	if err := StartOver(st, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(notePath); !os.IsNotExist(err) {
		t.Errorf("note should be removed by start-over, stat err = %v", err)
	}
	if _, ok, _ := st.GetImportSession(sid); ok {
		t.Error("session should be deleted by start-over")
	}
	proc, _ := st.ProcessedUUIDs()
	if proc["u-dune"] {
		t.Error("start-over should forget processed uuids")
	}
}

func TestBuildPreview(t *testing.T) {
	books := []calibre.Book{
		engBook("Dune", "u-dune", "111"),
		lnVol("Overlord", 1, "u-ov1"),
		lnVol("Overlord", 2, "u-ov2"),
	}
	p := BuildPreview(books, nil, map[string]bool{"u-dune": true})
	if p.AlreadyDone != 1 {
		t.Errorf("AlreadyDone = %d, want 1", p.AlreadyDone)
	}
	if p.LNSeries != 1 || p.LNVolumes != 2 {
		t.Errorf("LN counts = %d series / %d volumes, want 1/2", p.LNSeries, p.LNVolumes)
	}
	if p.RegularBooks != 0 {
		t.Errorf("RegularBooks = %d, want 0 (Dune already done)", p.RegularBooks)
	}
}

func TestWriteReport(t *testing.T) {
	dir := t.TempDir()
	items := []store.ImportItem{
		{Title: "Dune", State: "matched"},
		{Title: "Mystery Book", State: "unmatched"},
		{Title: "Wiedźmin - Ostatnie Życzenie", State: "matched", DuplicateOf: "Ostatnie Zyczenie"},
		{Title: "Broken", State: "errored", Error: "network"},
	}
	path, sum, err := WriteReport(dir, testToday, items)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Matched != 2 || sum.Unmatched != 1 || sum.Duplicates != 1 || sum.Errored != 1 {
		t.Fatalf("summary wrong: %+v", sum)
	}
	body := readFile(t, path)
	for _, want := range []string{
		"Import 2026-07-08",
		"[[Mystery Book]]",                                          // unmatched → wikilink
		"Possible duplicates",                                       // dup section header
		"duplicate of [[Ostatnie Zyczenie]]",                        // dup target wikilink
		"Broken — network",                                          // errored, plain (no staged note)
	} {
		if !contains(body, want) {
			t.Errorf("report missing %q", want)
		}
	}
	// A second write appends rather than overwrites.
	if _, _, err := WriteReport(dir, "2026-07-09", items); err != nil {
		t.Fatal(err)
	}
	body = readFile(t, path)
	if !strings.Contains(body, "Import 2026-07-08") || !strings.Contains(body, "Import 2026-07-09") {
		t.Error("second report should append, keeping both dated sections")
	}
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }
