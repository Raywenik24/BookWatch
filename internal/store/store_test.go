package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpen_migrateIdempotentAndSeedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Domain != "jnovels.com" {
		t.Fatalf("expected 1 seeded jnovels source, got %+v", srcs)
	}
	if len(srcs[0].Rules) != 5 {
		t.Errorf("expected 5 seeded rules, got %d", len(srcs[0].Rules))
	}
	st.Close()

	// Re-open: migrations must not re-run and seed must not double.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if srcs2, _ := st2.ListSources(); len(srcs2) != 1 {
		t.Errorf("seed ran twice on reopen: %d sources", len(srcs2))
	}
}

func TestUpsertBook_insertUpdateAndCoverPreserve(t *testing.T) {
	st := openTemp(t)
	id, err := st.UpsertBook("Title", "https://x/1", "/p/1.md", 2, "cover.jpg", "", nil, "ln", "")
	if err != nil {
		t.Fatal(err)
	}
	// Update with an empty cover must preserve the existing cover.
	id2, err := st.UpsertBook("Title v2", "https://x/1", "/p/1.md", 3, "", "", nil, "ln", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != id2 {
		t.Errorf("upsert by link should keep id: %d != %d", id, id2)
	}
	books, _ := st.ListBooks()
	if len(books) != 1 {
		t.Fatalf("expected 1 book, got %d", len(books))
	}
	b := books[0]
	if b.Volumes != 3 {
		t.Errorf("volumes not updated: %d", b.Volumes)
	}
	if b.Cover != "cover.jpg" {
		t.Errorf("empty cover cleared existing: %q", b.Cover)
	}
	if b.Title != "Title v2" {
		t.Errorf("title not updated: %q", b.Title)
	}
	// A non-empty cover does update.
	st.UpsertBook("Title v2", "https://x/1", "/p/1.md", 3, "new.png", "", nil, "ln", "")
	books, _ = st.ListBooks()
	if books[0].Cover != "new.png" {
		t.Errorf("cover not updated: %q", books[0].Cover)
	}
}

func TestBookExists(t *testing.T) {
	st := openTemp(t)
	if ex, _ := st.BookExists("https://x/none"); ex {
		t.Error("unexpected exists")
	}
	st.UpsertBook("T", "https://x/1", "", 1, "", "", nil, "ln", "")
	if ex, _ := st.BookExists("https://x/1"); !ex {
		t.Error("expected exists")
	}
}

func TestPendingUpdate_onePerBookAndApply(t *testing.T) {
	st := openTemp(t)
	bookID, _ := st.UpsertBook("T", "https://x/1", "/p.md", 2, "", "", nil, "ln", "")

	// Detecting twice must not stack — one pending row, refreshed.
	u1, err := st.UpsertPendingUpdate(bookID, 2, 3, "https://x/1")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := st.UpsertPendingUpdate(bookID, 2, 4, "https://x/1")
	if err != nil {
		t.Fatal(err)
	}
	if u1 != u2 {
		t.Errorf("re-detect should reuse the pending row: %d != %d", u1, u2)
	}
	if n, _ := st.CountPending(); n != 1 {
		t.Errorf("expected 1 pending, got %d", n)
	}
	pend, _ := st.ListPending()
	if len(pend) != 1 || pend[0].NewVolumes != 4 {
		t.Fatalf("pending not refreshed to 4: %+v", pend)
	}

	// Apply stamps the update + bumps the book.
	if err := st.MarkApplied(pend[0].ID, bookID, 4); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountPending(); n != 0 {
		t.Errorf("expected 0 pending after apply, got %d", n)
	}
	if books, _ := st.ListBooks(); books[0].Volumes != 4 {
		t.Errorf("book not bumped: %d", books[0].Volumes)
	}

	// A fresh detection after apply opens a NEW pending row.
	u3, _ := st.UpsertPendingUpdate(bookID, 4, 5, "https://x/1")
	if u3 == u2 {
		t.Error("post-apply detection should open a new row, not reuse the applied one")
	}
	if n, _ := st.CountPending(); n != 1 {
		t.Errorf("expected 1 new pending, got %d", n)
	}
}

// Pragmas must apply to every pooled connection, not just the one Open ran on.
// Holding c1 forces the pool to open a second physical connection for c2; with
// the old once-via-Exec approach that second connection would report
// foreign_keys=0 / busy_timeout=0. Regression guard for the DSN-pragma fix.
func TestOpen_pragmasOnEveryConnection(t *testing.T) {
	st := openTemp(t)
	st.db.SetMaxOpenConns(3)
	ctx := context.Background()

	c1, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := st.db.Conn(ctx) // forced new conn: c1 is still held
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	for i, c := range []*sql.Conn{c1, c2} {
		var fk, bt int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Errorf("conn %d: foreign_keys=%d, want 1", i, fk)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
			t.Fatal(err)
		}
		if bt != 5000 {
			t.Errorf("conn %d: busy_timeout=%d, want 5000", i, bt)
		}
	}
}

// Cascade delete must work even on a connection that did not run Open's pragmas.
func TestDeleteBook_cascadesOnFreshConnection(t *testing.T) {
	st := openTemp(t)
	st.db.SetMaxOpenConns(2)
	// Pin one connection so the delete below is forced onto a different one.
	pin, err := st.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()

	bookID, _ := st.UpsertBook("T", "https://x/casc", "/p.md", 2, "", "", nil, "ln", "")
	st.UpsertPendingUpdate(bookID, 2, 3, "https://x/casc")
	if err := st.DeleteBook(bookID); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountPending(); n != 0 {
		t.Errorf("cascade did not fire on a fresh connection: %d updates remain", n)
	}
}

func TestDeleteBook_cascadesUpdates(t *testing.T) {
	st := openTemp(t)
	bookID, _ := st.UpsertBook("T", "https://x/1", "/p.md", 2, "", "", nil, "ln", "")
	st.UpsertPendingUpdate(bookID, 2, 3, "https://x/1")
	if err := st.DeleteBook(bookID); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountPending(); n != 0 {
		t.Errorf("updates not cascaded on delete: %d remain", n)
	}
	if books, _ := st.ListBooks(); len(books) != 0 {
		t.Errorf("book not deleted: %d", len(books))
	}
}

func TestRuns(t *testing.T) {
	st := openTemp(t)
	id, err := st.StartRun()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(id, 10, 2, 1, "10 notes, 2 updates, 1 errors"); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.ListRuns(10)
	if len(runs) != 1 || runs[0].Checked != 10 || runs[0].Updated != 2 || runs[0].Errors != 1 {
		t.Fatalf("run not recorded: %+v", runs)
	}
	if runs[0].FinishedAt == "" {
		t.Error("finished_at not stamped")
	}
}

func TestSettings(t *testing.T) {
	st := openTemp(t)
	if _, ok, _ := st.GetSetting("k"); ok {
		t.Error("unexpected setting present")
	}
	st.SetSetting("k", "v1")
	st.SetSetting("k", "v2") // upsert
	v, ok, _ := st.GetSetting("k")
	if !ok || v != "v2" {
		t.Errorf("got %q ok=%v", v, ok)
	}
	if all, _ := st.AllSettings(); all["k"] != "v2" {
		t.Errorf("AllSettings: %+v", all)
	}
}

func TestSourcesAndRulesCRUD(t *testing.T) {
	st := openTemp(t)
	id, err := st.UpsertSource("Foo", "foo.com", "", true) // empty strategy defaults to "rules"
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertRule(id, "title", "h1", "", "")
	st.UpsertRule(id, "title", "h2", "", "") // upsert same field

	var foo *Source
	srcs, _ := st.ListSources()
	for i := range srcs {
		if srcs[i].Domain == "foo.com" {
			foo = &srcs[i]
		}
	}
	if foo == nil {
		t.Fatal("source not found")
	}
	if foo.Strategy != "rules" {
		t.Errorf("default strategy: %q", foo.Strategy)
	}
	if len(foo.Rules) != 1 || foo.Rules[0].Selector != "h2" {
		t.Errorf("rule not upserted in place: %+v", foo.Rules)
	}

	if err := st.DeleteSource(id); err != nil {
		t.Fatal(err)
	}
	srcs, _ = st.ListSources()
	for _, s := range srcs {
		if s.Domain == "foo.com" {
			t.Error("source not deleted")
		}
	}
}

func TestEvents_newestFirst(t *testing.T) {
	st := openTemp(t)
	st.LogEvent("add", "Added X")
	st.LogEvent("untrack", "Untracked Y")
	evs, _ := st.ListEvents(10)
	if len(evs) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evs))
	}
	if evs[0].Kind != "untrack" || evs[1].Kind != "add" {
		t.Errorf("events not newest-first: %+v", evs)
	}
}

func TestUpsertBook_kindAndAuthor(t *testing.T) {
	st := openTemp(t)

	// LN book carries kind=ln.
	lnID, err := st.UpsertBook("Mushoku Tensei", "https://jnovels.com/mt", "/mt.md", 25, "c.jpg", "Queue", nil, "ln", "Rifujin")
	if err != nil {
		t.Fatal(err)
	}
	// Book note carries kind=book.
	bookID, err := st.UpsertBook("Rich Dad Poor Dad", "https://openlibrary.org/works/OL20749838W", "/rdpd.md", 0, "", "Queue", nil, "book", "Robert T. Kiyosaki")
	if err != nil {
		t.Fatal(err)
	}

	books, _ := st.ListBooks()
	byID := map[int64]Book{}
	for _, b := range books {
		byID[b.ID] = b
	}

	if byID[lnID].Kind != "ln" {
		t.Errorf("ln kind: %q", byID[lnID].Kind)
	}
	if byID[lnID].Author != "Rifujin" {
		t.Errorf("ln author: %q", byID[lnID].Author)
	}
	if byID[bookID].Kind != "book" {
		t.Errorf("book kind: %q", byID[bookID].Kind)
	}
	if byID[bookID].Author != "Robert T. Kiyosaki" {
		t.Errorf("book author: %q", byID[bookID].Author)
	}

	// kind must not be overwritten on upsert.
	st.UpsertBook("Rich Dad Poor Dad", "https://openlibrary.org/works/OL20749838W", "/rdpd.md", 0, "", "", nil, "ln", "")
	books, _ = st.ListBooks()
	for _, b := range books {
		if b.ID == bookID && b.Kind != "book" {
			t.Errorf("kind overwritten on upsert: %q", b.Kind)
		}
	}
}

func TestTrackerCRUD(t *testing.T) {
	st := openTemp(t)

	id, err := st.UpsertTracker("author", "Graham Masterton", "/authors/OL123456A", "OL20W", "1975", "eng", false)
	if err != nil {
		t.Fatal(err)
	}

	// Upsert same ol_key updates fields, keeps id.
	id2, err := st.UpsertTracker("author", "Graham Masterton", "/authors/OL123456A", "OL21W", "1976", "eng", false)
	if err != nil {
		t.Fatal(err)
	}
	if id != id2 {
		t.Errorf("upsert should keep same id: %d != %d", id, id2)
	}

	trackers, err := st.ListTrackers()
	if err != nil {
		t.Fatal(err)
	}
	if len(trackers) != 1 {
		t.Fatalf("expected 1 tracker, got %d", len(trackers))
	}
	tr := trackers[0]
	if tr.BaselineWorkID != "OL21W" {
		t.Errorf("baseline_work_id not updated: %q", tr.BaselineWorkID)
	}
	if tr.Name != "Graham Masterton" {
		t.Errorf("name: %q", tr.Name)
	}

	if err := st.DeleteTracker(id); err != nil {
		t.Fatal(err)
	}
	if trs, _ := st.ListTrackers(); len(trs) != 0 {
		t.Errorf("tracker not deleted")
	}
}

// TestTrackerWatchPolishTranslation covers #46's opt-in flag: set on create,
// toggled via UpdateTrackerBaseline, round-trips through ListTrackers.
func TestTrackerWatchPolishTranslation(t *testing.T) {
	st := openTemp(t)

	id, err := st.UpsertTracker("author", "Lee Child", "/authors/OL1A", "W1", "2010", "eng", true)
	if err != nil {
		t.Fatal(err)
	}
	trackers, _ := st.ListTrackers()
	if !trackers[0].WatchPolishTranslation {
		t.Fatal("watch_pl_translation not set on create")
	}

	if err := st.UpdateTrackerBaseline(id, "W1", "2010", "eng", false); err != nil {
		t.Fatal(err)
	}
	trackers, _ = st.ListTrackers()
	if trackers[0].WatchPolishTranslation {
		t.Fatal("watch_pl_translation not cleared by UpdateTrackerBaseline")
	}
}

func TestSeenWorks(t *testing.T) {
	st := openTemp(t)
	id, _ := st.UpsertTracker("author", "A", "/authors/OL1A", "", "", "eng", false)

	if err := st.AddSeenWork(id, "OL100W"); err != nil {
		t.Fatal(err)
	}
	// Duplicate must not error.
	if err := st.AddSeenWork(id, "OL100W"); err != nil {
		t.Fatal(err)
	}
	st.AddSeenWork(id, "OL200W")

	ids, err := st.SeenWorkIDs(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 seen works, got %d: %v", len(ids), ids)
	}

	// Deleting tracker cascades to seen_works.
	st.DeleteTracker(id)
	ids, _ = st.SeenWorkIDs(id)
	if len(ids) != 0 {
		t.Errorf("cascade did not remove seen_works: %d remain", len(ids))
	}
}

func TestReleases(t *testing.T) {
	st := openTemp(t)
	tid, _ := st.UpsertTracker("author", "A", "/authors/OL1A", "", "", "eng", false)

	rid, err := st.UpsertRelease(tid, "OL300W", "The Book", "A. Author", "2024", "https://cdn/c.jpg", "")
	if err != nil {
		t.Fatal(err)
	}

	// Re-upsert updates metadata.
	rid2, err := st.UpsertRelease(tid, "OL300W", "The Book (Updated)", "A. Author", "2024", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rid != rid2 {
		t.Errorf("upsert should reuse row: %d != %d", rid, rid2)
	}

	rels, err := st.ListReleases(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 release, got %d", len(rels))
	}
	if rels[0].Title != "The Book (Updated)" {
		t.Errorf("title not updated: %q", rels[0].Title)
	}

	// Dismiss hides from ListReleases.
	if err := st.DismissRelease(rid); err != nil {
		t.Fatal(err)
	}
	rels, _ = st.ListReleases(10)
	if len(rels) != 0 {
		t.Errorf("dismissed release still returned: %d", len(rels))
	}

	// Cascade: deleting tracker removes releases.
	st.UpsertRelease(tid, "OL301W", "Another", "A", "2025", "", "")
	st.DeleteTracker(tid)
	// Can't query directly since no API for all-releases, but open a new DB copy
	// and verify via raw SQL.
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM releases`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("cascade did not remove releases: %d remain", n)
	}
}

// TestPrimaryReleases covers #46's kind split: PrimaryReleases returns only
// kind="" rows, and a "translation-of" release round-trips its Kind field.
func TestPrimaryReleases(t *testing.T) {
	st := openTemp(t)
	tid, _ := st.UpsertTracker("author", "A", "/authors/OL1A", "", "", "eng", true)

	if _, err := st.UpsertRelease(tid, "OL400W", "The Book", "A", "2024", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRelease(tid, "pl-tr:OL400W", "Ksiazka", "A", "2024", "", "translation-of"); err != nil {
		t.Fatal(err)
	}

	primaries, err := st.PrimaryReleases(tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(primaries) != 1 || primaries[0].WorkID != "OL400W" {
		t.Fatalf("expected only the primary release, got %+v", primaries)
	}

	rels, _ := st.ListReleases(10)
	var translation *Release
	for i := range rels {
		if rels[i].Kind == "translation-of" {
			translation = &rels[i]
		}
	}
	if translation == nil || translation.WorkID != "pl-tr:OL400W" {
		t.Fatalf("expected translation-of release to round-trip its kind, got %+v", rels)
	}
}

func TestBookCoverAndTitle(t *testing.T) {
	st := openTemp(t)
	if c, _ := st.BookCover(999); c != "" {
		t.Error("expected empty cover for missing book")
	}
	if ti, _ := st.BookTitle(999); ti != "" {
		t.Error("expected empty title for missing book")
	}
	id, _ := st.UpsertBook("My Book", "https://x/1", "", 1, "c.jpg", "", nil, "ln", "")
	if c, _ := st.BookCover(id); c != "c.jpg" {
		t.Errorf("cover: %q", c)
	}
	if ti, _ := st.BookTitle(id); ti != "My Book" {
		t.Errorf("title: %q", ti)
	}
}
