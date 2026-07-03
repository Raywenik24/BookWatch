package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bookwatch/internal/provider"
	"bookwatch/internal/scraper"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

func init() { scraper.AllowPrivateHosts = true } // httptest binds to loopback

func novelHTML(vols int) string {
	var b strings.Builder
	for i := 1; i <= vols; i++ {
		fmt.Fprintf(&b, "<li>Download VOLUME %d Epub</li>", i)
	}
	return `<!doctype html><html><body>
<h1 class="post-title entry-title">N Epub</h1>
<div class="featured-media"><img src="/c.jpg"></div>
<div class="synopsis-description"><p>D.</p></div>
<ol>` + b.String() + `</ol></body></html>`
}

func writeNote(t *testing.T, dir, name, link string, vols int) string {
	t.Helper()
	content := fmt.Sprintf("---\nLink: %s\nVolumes: %d\ntags:\n  - \"#LightNovel\"\nTemplate_used: LightNovelTemplate\n---\n### %s\n", link, vols, name)
	p := filepath.Join(dir, name+".md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRunCheck_detectOnlyThenApply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(novelHTML(3)))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	notePath := writeNote(t, vaultDir, "Book A", srv.URL+"/a", 2)
	st := openStore(t)
	sc := scraper.New("t", 5*time.Second)

	// Detect-only: finds the bump but writes nothing.
	sum, err := RunCheck(sc, st, nil, nil, vaultDir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Checked != 1 || sum.Updated != 1 || sum.Errors != 0 {
		t.Fatalf("summary: %+v", sum)
	}
	if raw, _ := os.ReadFile(notePath); !strings.Contains(string(raw), "Volumes: 2") {
		t.Errorf("detect-only must not write the vault:\n%s", raw)
	}
	if n, _ := st.CountPending(); n != 1 {
		t.Errorf("expected 1 pending update, got %d", n)
	}
	if books, _ := st.ListBooks(); books[0].Volumes != 2 {
		t.Errorf("detect-only must not bump the book: %d", books[0].Volumes)
	}

	// Apply writes the stored number to the note and bumps the book.
	pending, err := st.ListPending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending: %v %+v", err, pending)
	}
	res, err := ApplyPending(st, vault.Today(), []int64{pending[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 1 || res.Failed != 0 {
		t.Fatalf("apply: %+v", res)
	}
	if raw, _ := os.ReadFile(notePath); !strings.Contains(string(raw), "Volumes: 3") {
		t.Errorf("apply should write Volumes: 3:\n%s", raw)
	}
	if books, _ := st.ListBooks(); books[0].Volumes != 3 {
		t.Errorf("book not bumped after apply: %d", books[0].Volumes)
	}
	if n, _ := st.CountPending(); n != 0 {
		t.Errorf("pending not cleared after apply: %d", n)
	}
}

func TestRunCheck_logsScrapeAnomaly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 but no volume list → count 0 → suspicious, not an error.
		w.Write([]byte(`<html><body><h1 class="post-title entry-title">N</h1></body></html>`))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	writeNote(t, vaultDir, "Broken", srv.URL+"/x", 4)
	st := openStore(t)
	sc := scraper.New("t", 5*time.Second)

	sum, err := RunCheck(sc, st, nil, nil, vaultDir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Suspicious != 1 || sum.Updated != 0 || sum.Errors != 0 {
		t.Fatalf("expected 1 suspicious / 0 updates / 0 errors: %+v", sum)
	}
	evs, _ := st.ListEvents(10)
	found := false
	for _, e := range evs {
		if e.Kind == "anomaly" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an anomaly event to be logged, got %+v", evs)
	}
	// The bad read must not change the recorded volume count.
	if books, _ := st.ListBooks(); len(books) != 1 || books[0].Volumes != 4 {
		t.Errorf("suspicious scrape must not change stored volumes: %+v", books)
	}
}

func writeNoteWithStatus(t *testing.T, dir, name, link string, vols, readVols int, status string) string {
	t.Helper()
	content := fmt.Sprintf(
		"---\nLink: %s\nVolumes: %d\nRead Volumes: %d\nStatus: %s\ntags:\n  - \"#LightNovel\"\nTemplate_used: LightNovelTemplate\n---\n### %s\n",
		link, vols, readVols, status, name)
	p := filepath.Join(dir, name+".md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetermineStatusCorrection(t *testing.T) {
	cases := []struct {
		name   string
		e      vault.Entry
		hasNew bool
		want   string
	}{
		{"dropped ignored", vault.Entry{Status: "Dropped", Volumes: 5, ReadVolumes: 3, HasReadVolumes: true}, false, ""},
		{"hasNew completed -> queue", vault.Entry{Status: "Completed", Volumes: 5, ReadVolumes: 5, HasReadVolumes: true}, true, "Queue"},
		{"readVols < vols completed -> queue", vault.Entry{Status: "Completed", Volumes: 5, ReadVolumes: 3, HasReadVolumes: true}, false, "Queue"},
		{"readVols == vols queue -> completed", vault.Entry{Status: "Queue", Volumes: 5, ReadVolumes: 5, HasReadVolumes: true}, false, "Completed"},
		{"readVols > vols no change", vault.Entry{Status: "Completed", Volumes: 3, ReadVolumes: 5, HasReadVolumes: true}, false, ""},
		{"blank readVols treated as 0 completed", vault.Entry{Status: "Completed", Volumes: 3, HasReadVolumes: false}, false, "Queue"},
		{"blank readVols treated as 0 queue no change", vault.Entry{Status: "Queue", Volumes: 5, HasReadVolumes: false}, false, ""},
		{"hasNew queue no change", vault.Entry{Status: "Queue", Volumes: 5, ReadVolumes: 5, HasReadVolumes: true}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := determineStatusCorrection(tc.e, tc.hasNew)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRunCheck_statusAutoCorrection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(novelHTML(2)))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	sc := scraper.New("t", 5*time.Second)
	st := openStore(t)

	// HasNew + Completed → Queue
	pathHasNew := writeNoteWithStatus(t, vaultDir, "HasNewCompleted", srv.URL+"/a", 1, 1, "Completed")
	// !HasNew, ReadVols(1) < Vols(2), Completed → Queue
	pathLowRead := writeNoteWithStatus(t, vaultDir, "LowRead", srv.URL+"/b", 2, 1, "Completed")
	// !HasNew, ReadVols == Vols, Queue → Completed
	pathQueueDone := writeNoteWithStatus(t, vaultDir, "QueueDone", srv.URL+"/c", 2, 2, "Queue")
	// Dropped → never touched
	pathDropped := writeNoteWithStatus(t, vaultDir, "Dropped", srv.URL+"/d", 2, 2, "Dropped")

	if _, err := RunCheck(sc, st, nil, nil, vaultDir, false, nil); err != nil {
		t.Fatal(err)
	}

	checkContains := func(path, want string) {
		t.Helper()
		raw, _ := os.ReadFile(path)
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s: expected %q in:\n%s", filepath.Base(path), want, raw)
		}
	}

	checkContains(pathHasNew, "  - Queue")
	checkContains(pathLowRead, "  - Queue")
	checkContains(pathQueueDone, "  - Completed")
	// Dropped note should not have been rewritten to list format (no correction applied)
	checkContains(pathDropped, "Dropped")

	evs, _ := st.ListEvents(20)
	fixCount := 0
	for _, e := range evs {
		if e.Kind == "status-fix" {
			fixCount++
		}
	}
	if fixCount != 3 {
		t.Errorf("expected 3 status-fix events, got %d: %+v", fixCount, evs)
	}
}

func TestRunCheck_prunesOnlyMissingNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(novelHTML(1)))
	}))
	defer srv.Close()

	vaultDir := t.TempDir()
	writeNote(t, vaultDir, "Keep", srv.URL+"/keep", 1) // scanned + kept
	st := openStore(t)
	sc := scraper.New("t", 5*time.Second)

	// Stale: not in the scan (note gone) → must be pruned.
	st.UpsertBook("Stale", "https://nope/x", filepath.Join(vaultDir, "gone.md"), 1, "", "", nil, "ln", "")
	// Not in the scan (note exists but lacks Template_used filter) → also pruned.
	existing := filepath.Join(vaultDir, "untagged.md")
	os.WriteFile(existing, []byte("not a LN note"), 0o644)
	st.UpsertBook("OnDisk", "https://nope/y", existing, 1, "", "", nil, "ln", "")

	if _, err := RunCheck(sc, st, nil, nil, vaultDir, false, nil); err != nil {
		t.Fatal(err)
	}

	links := map[string]bool{}
	books, _ := st.ListBooks()
	for _, b := range books {
		links[b.Link] = true
	}
	if links["https://nope/x"] {
		t.Error("stale book with a missing note should have been pruned")
	}
	if links["https://nope/y"] {
		t.Error("book whose note lacks Template_used should have been pruned")
	}
}

// fakeProvider is a minimal provider.Provider stub for pollTrackers tests.
type fakeProvider struct {
	works   []provider.Work
	details map[string]provider.Work
	err     error
}

func (f *fakeProvider) SearchByTitle(string) ([]provider.Candidate, error) { return nil, nil }
func (f *fakeProvider) AuthorSearch(string) ([]provider.Author, error)     { return nil, nil }
func (f *fakeProvider) WorkByID(string) (provider.Candidate, error)        { return provider.Candidate{}, nil }

func (f *fakeProvider) AuthorWorks(string) ([]provider.Work, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.works, nil
}

func (f *fakeProvider) WorkDetail(id string) (provider.Work, error) {
	return f.details[id], nil
}

func (f *fakeProvider) WorkEditions(id string) ([]provider.Edition, error) {
	return f.details[id].Editions, nil
}

func TestPollTrackers_normalizesReleases(t *testing.T) {
	st := openStore(t)

	trackerID, err := st.UpsertTracker("author", "Graham Masterton", "OL123A", "W1", "2000", "eng", false)
	if err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{
		works: []provider.Work{
			{WorkID: "W1", Title: "Baseline Book", FirstPubYear: 2000},    // the baseline itself
			{WorkID: "W2", Title: "Old Backlist", FirstPubYear: 1995},     // before baseline
			{WorkID: "W3", Title: "New Book", FirstPubYear: 2005},         // should surface
			{WorkID: "W4", Title: "New Book Box Set", FirstPubYear: 2006}, // bundle, filtered
			{WorkID: "W5", Title: "Foreign Only", FirstPubYear: 2007},     // no eng edition
			{WorkID: "W6", Title: "Tie Book", FirstPubYear: 2000},         // same year as baseline: kept
		},
		details: map[string]provider.Work{
			"W3": {WorkID: "W3", Title: "New Book", Editions: []provider.Edition{{Language: "eng", CoverURL: "c3"}}},
			"W4": {WorkID: "W4", Title: "New Book Box Set", Editions: []provider.Edition{{Language: "eng"}}},
			"W5": {WorkID: "W5", Title: "Foreign Only", Editions: []provider.Edition{{Language: "fre"}}},
			"W6": {WorkID: "W6", Title: "Tie Book", Editions: []provider.Edition{{Language: "eng", CoverURL: "c6"}}},
		},
	}

	checked, newReleases, errs := pollTrackers(fp, nil, st, nil)
	if checked != 1 || errs != 0 {
		t.Fatalf("checked=%d errs=%d, want 1/0", checked, errs)
	}
	if newReleases != 2 {
		t.Fatalf("newReleases=%d, want 2 (W3, W6)", newReleases)
	}

	releases, err := st.ListReleases(10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range releases {
		got[r.WorkID] = true
	}
	if !got["W3"] || !got["W6"] {
		t.Errorf("expected W3 and W6 surfaced, got %+v", releases)
	}
	if got["W1"] || got["W2"] || got["W4"] || got["W5"] {
		t.Errorf("baseline/backlist/bundle/foreign-only works must not surface: %+v", releases)
	}

	// Re-polling must not re-surface a dismissed release.
	st.DismissRelease(releases[0].ID)
	if _, _, errs := pollTrackers(fp, nil, st, nil); errs != 0 {
		t.Fatalf("re-poll errs=%d", errs)
	}
	live, _ := st.ListReleases(10)
	if len(live) != 1 {
		t.Errorf("dismissed release re-surfaced: %+v", live)
	}

	// Provider failure on one tracker counts as a tracking error, not a scan error.
	fpErr := &fakeProvider{err: fmt.Errorf("openlibrary down")}
	checked, _, errs = pollTrackers(fpErr, nil, st, nil)
	if checked != 1 || errs != 1 {
		t.Fatalf("checked=%d errs=%d, want 1/1 on provider failure", checked, errs)
	}

	_ = trackerID
}

// fakePolishSource stubs provider.PolishSource (Lubimyczytać) for the poll's
// Polish passes. matches keys off title, for the translation-watch pass (#46).
type fakePolishSource struct {
	path    string
	works   []provider.Work
	err     error
	matches map[string]provider.Match
}

func (f *fakePolishSource) AuthorSearch(string) string { return f.path }
func (f *fakePolishSource) AuthorWorks(string) ([]provider.Work, error) {
	return f.works, f.err
}
func (f *fakePolishSource) MatchWork(title, _ string, _ []string) provider.Match {
	return f.matches[title]
}

func TestPollTrackers_polishReleasesFromLubimyczytac(t *testing.T) {
	st := openStore(t)
	if _, err := st.UpsertTracker("author", "Peter V. Brett", "OL18930A", "W1", "2010", "pol", false); err != nil {
		t.Fatal(err)
	}

	// OL surfaces nothing Polish (the language:null fragmentation the issue
	// describes): its only new work has no pol edition.
	ol := &fakeProvider{
		works:   []provider.Work{{WorkID: "OLnew", Title: "The Core", FirstPubYear: 2017}},
		details: map[string]provider.Work{"OLnew": {WorkID: "OLnew", Editions: []provider.Edition{{Language: "eng"}}}},
	}
	// Lubimyczytać lists the Polish editions cleanly.
	lc := &fakePolishSource{
		path: "/autor/18930/peter-v-brett",
		works: []provider.Work{
			{WorkID: "cykl:1594", Title: "Malowany człowiek", FirstPubYear: 2021, CoverURL: "pl1.jpg"},
			{WorkID: "cykl:1594#2", Title: "Pustynna Włócznia", FirstPubYear: 2022, CoverURL: "pl2.jpg"},
			{WorkID: "lc:99", Title: "Stary Backlist", FirstPubYear: 2005},  // before baseline -> filtered
			{WorkID: "lc:100", Title: "Zestaw Box Set", FirstPubYear: 2023}, // bundle -> filtered
		},
	}

	checked, newReleases, errs := pollTrackers(ol, lc, st, nil)
	if checked != 1 || errs != 0 {
		t.Fatalf("checked=%d errs=%d, want 1/0", checked, errs)
	}
	if newReleases != 2 {
		t.Fatalf("newReleases=%d, want 2 Polish editions", newReleases)
	}
	got := map[string]bool{}
	rel, _ := st.ListReleases(10)
	for _, r := range rel {
		got[r.WorkID] = true
	}
	if !got["cykl:1594"] || !got["cykl:1594#2"] {
		t.Errorf("both Polish editions should surface, got %+v", rel)
	}
	if got["lc:99"] || got["lc:100"] {
		t.Errorf("pre-baseline / bundle Polish works must be filtered: %+v", rel)
	}

	// The Polish pass is best-effort: a Lubimyczytać failure is one tracking error,
	// and an author simply absent from the site is not an error.
	lcErr := &fakePolishSource{path: "/autor/1/x", err: fmt.Errorf("lubimyczytac down")}
	if _, _, errs := pollTrackers(ol, lcErr, st, nil); errs != 1 {
		t.Errorf("a Lubimyczytać failure should count as 1 tracking error, got %d", errs)
	}
	lcAbsent := &fakePolishSource{path: ""}
	if _, _, errs := pollTrackers(ol, lcAbsent, st, nil); errs != 0 {
		t.Errorf("an author absent from Lubimyczytać is not an error, got %d", errs)
	}
}

// TestPollTrackers_polishTranslationWatch covers #46: an opt-in flag on an
// English-catalog tracker that, once a book surfaces in English, separately
// checks Lubimyczytać for a Polish edition of that specific book — by title,
// not a bibliography scan — and surfaces a hit as its own "translation-of"
// release.
func TestPollTrackers_polishTranslationWatch(t *testing.T) {
	st := openStore(t)
	trackerID, err := st.UpsertTracker("author", "Lee Child", "OL1A", "W1", "2010", "eng", true)
	if err != nil {
		t.Fatal(err)
	}

	ol := &fakeProvider{
		works:   []provider.Work{{WorkID: "OLnew", Title: "The Sentinel", FirstPubYear: 2020}},
		details: map[string]provider.Work{"OLnew": {WorkID: "OLnew", Editions: []provider.Edition{{Language: "eng"}}}},
	}
	lc := &fakePolishSource{
		matches: map[string]provider.Match{
			"The Sentinel": {WorkID: "lc:555", Title: "Wartownik", CoverURL: "pl.jpg", Author: "Lee Child", Found: true},
		},
	}

	checked, newReleases, errs := pollTrackers(ol, lc, st, nil)
	if checked != 1 || errs != 0 {
		t.Fatalf("checked=%d errs=%d, want 1/0", checked, errs)
	}
	if newReleases != 2 {
		t.Fatalf("newReleases=%d, want 2 (English release + Polish translation)", newReleases)
	}
	rel, _ := st.ListReleases(10)
	var primary, translation *store.Release
	for i := range rel {
		switch rel[i].Kind {
		case "":
			primary = &rel[i]
		case "translation-of":
			translation = &rel[i]
		}
	}
	if primary == nil || primary.WorkID != "OLnew" {
		t.Fatalf("expected primary English release for OLnew, got %+v", rel)
	}
	if translation == nil || translation.WorkID != "pl-tr:OLnew" || translation.Title != "Wartownik" {
		t.Fatalf("expected translation-of release keyed off the primary work, got %+v", rel)
	}

	// Re-polling must not insert the translation again (it's already been
	// found — the point of `seen` picking it back up via ReleaseWorkIDs).
	if _, newReleases, _ := pollTrackers(ol, lc, st, nil); newReleases != 0 {
		t.Errorf("a re-poll after the translation is already found should surface nothing new, got %d", newReleases)
	}

	_ = trackerID
}

// TestPollTrackers_polishTranslationWatch_gated confirms the pass only runs
// when both the flag is set AND the tracker isn't already Polish-catalog —
// the reverse direction is explicitly out of scope for #46.
func TestPollTrackers_polishTranslationWatch_gated(t *testing.T) {
	st := openStore(t)
	ol := &fakeProvider{
		works:   []provider.Work{{WorkID: "OLnew", Title: "The Sentinel", FirstPubYear: 2020}},
		details: map[string]provider.Work{"OLnew": {WorkID: "OLnew", Editions: []provider.Edition{{Language: "eng"}}}},
	}
	lc := &fakePolishSource{
		matches: map[string]provider.Match{
			"The Sentinel": {WorkID: "lc:555", Title: "Wartownik", Found: true},
		},
	}

	// Flag off: English release only.
	if _, err := st.UpsertTracker("author", "A", "OL1A", "W1", "2010", "eng", false); err != nil {
		t.Fatal(err)
	}
	if _, newReleases, _ := pollTrackers(ol, lc, st, nil); newReleases != 1 {
		t.Errorf("flag off should surface only the English release, got %d new", newReleases)
	}

	// Flag on but catalog is already Polish: the LC bibliography pass owns
	// this tracker instead, and it returns nothing here (no path/works set).
	st2 := openStore(t)
	if _, err := st2.UpsertTracker("author", "B", "OL2A", "W2", "2010", "pol", true); err != nil {
		t.Fatal(err)
	}
	if _, newReleases, _ := pollTrackers(ol, lc, st2, nil); newReleases != 0 {
		t.Errorf("pol-catalog tracker should not run the translation-watch pass, got %d new", newReleases)
	}
}
