package provider

import (
	"strings"
	"testing"
)

// fakeMatcher resolves by ISBN (mirroring the real GRClient), standing in for
// live Goodreads so ClusterWorks runs entirely offline.
type fakeMatcher struct {
	byISBN map[string]Match
	calls  int
}

func (f *fakeMatcher) MatchWork(_, author string, isbns []string) Match {
	f.calls++
	for _, isbn := range isbns {
		if m, ok := f.byISBN[isbn]; ok {
			if author == "" || sameAuthor(author, m.Author) {
				return m
			}
		}
	}
	return Match{}
}

// fakeTitleMatcher resolves by normalized title (mirroring the real LCClient),
// standing in for live Lubimyczytać so the Polish pass runs offline.
type fakeTitleMatcher struct {
	byTitle map[string]Match
	calls   int
}

func (f *fakeTitleMatcher) MatchWork(title, author string, _ []string) Match {
	f.calls++
	if m, ok := f.byTitle[normTitle(title)]; ok {
		if author == "" || sameAuthor(author, m.Author) {
			return m
		}
	}
	return Match{}
}

// The Brett cluster as live data actually behaves: English/French/Spanish share
// Goodreads work 6589794 (Goodreads' normal model — one work spans every
// translated edition); Polish is a separate Goodreads work; one OL work has a
// dirty ISBN that resolves to a different author and must not merge. English
// editions are reliably OL-tagged; French/Spanish commonly aren't (#45) —
// mirrored here so the language-bucket guard has something real to bite on.
func brettWorks() []Work {
	return []Work{
		{WorkID: "OL1W", Title: "The Warded Man", FirstPubYear: 2008, CoverURL: "warded.jpg", Language: "eng", ISBNs: []string{"EN1"}},
		{WorkID: "OL2W", Title: "Le Cycle des démons", FirstPubYear: 2009, CoverURL: "", ISBNs: []string{"FR1"}},
		{WorkID: "OL3W", Title: "El hombre marcado", FirstPubYear: 2009, CoverURL: "", ISBNs: []string{"ES1"}},
		{WorkID: "OL4W", Title: "Malowany człowiek", FirstPubYear: 2010, CoverURL: "", ISBNs: []string{"PL1"}},
		{WorkID: "OL5W", Title: "The Desert Spear", FirstPubYear: 2010, CoverURL: "spear.jpg", Language: "eng", ISBNs: []string{"EN2"}},
	}
}

func brettMatcher() *fakeMatcher {
	demon := Match{Found: true, WorkID: "6589794", CoverURL: "gr-warded.jpg", Author: "Peter V. Brett"}
	return &fakeMatcher{byISBN: map[string]Match{
		"EN1": demon,
		"FR1": demon,
		"ES1": demon,
		"PL1": {Found: true, WorkID: "21446513", CoverURL: "gr-polish.jpg", Author: "Peter V. Brett"}, // separate work
		"EN2": {Found: true, WorkID: "5738618", CoverURL: "gr-spear.jpg", Author: "Peter V. Brett"},
	}}
}

func TestClusterWorksCollapsesTranslations(t *testing.T) {
	got := ClusterWorks(brettWorks(), "Peter V. Brett", brettMatcher(), nil, 100, 100)
	// Goodreads shares one work id (6589794) across EN/FR/ES, but the language
	// bucket keeps English split from French+Spanish (which merge with each
	// other, both untagged) so a translation is never absorbed into the
	// English tile (#45): English, French+Spanish, Polish, Desert Spear -> 4.
	if len(got) != 4 {
		t.Fatalf("want 4 clusters (English bk1, French+Spanish bk1, Polish bk1, Desert Spear), got %d: %+v", len(got), got)
	}
	var demon *Work
	for i := range got {
		if got[i].WorkID == "OL1W" {
			demon = &got[i]
		}
	}
	if demon == nil {
		t.Fatalf("English (OL1W) should be its own tile, not absorbed into another language: %+v", got)
	}
	if demon.Title != "The Warded Man" || demon.FirstPubYear != 2008 {
		t.Errorf("English tile should keep its own title/year: got %q %d", demon.Title, demon.FirstPubYear)
	}
	// Polish must still be present as its own tile.
	var polish bool
	for _, w := range got {
		if strings.Contains(w.Title, "Malowany") {
			polish = true
		}
	}
	if !polish {
		t.Error("Polish edition should remain a separate tile (Goodreads files it as a separate work)")
	}
}

func TestClusterWorksBackfillsCoverWithinLanguage(t *testing.T) {
	// Two coverless, untagged translations sharing a GR work id: same language
	// bucket (both unknown) -> they still collapse and inherit the GR cover.
	works := []Work{
		{WorkID: "OL2W", Title: "Le Cycle des démons", FirstPubYear: 2009, CoverURL: "", ISBNs: []string{"FR1"}},
		{WorkID: "OL3W", Title: "El hombre marcado", FirstPubYear: 2009, CoverURL: "", ISBNs: []string{"ES1"}},
	}
	got := ClusterWorks(works, "Peter V. Brett", brettMatcher(), nil, 100, 100)
	if len(got) != 1 {
		t.Fatalf("FR+ES share a work and a language bucket -> 1 cluster, got %d", len(got))
	}
	if got[0].CoverURL != "gr-warded.jpg" {
		t.Errorf("coverless cluster should backfill the Goodreads cover, got %q", got[0].CoverURL)
	}
}

func TestClusterWorksDoesNotMergeAcrossLanguages(t *testing.T) {
	// Same GR work id, but explicitly tagged in two different languages —
	// must never merge, even though Goodreads considers them one work (#45).
	works := []Work{
		{WorkID: "OL1W", Title: "The Warded Man", FirstPubYear: 2008, Language: "eng", ISBNs: []string{"EN1"}},
		{WorkID: "OL2W", Title: "Le Cycle des démons", FirstPubYear: 2009, Language: "fre", ISBNs: []string{"FR1"}},
	}
	got := ClusterWorks(works, "Peter V. Brett", brettMatcher(), nil, 100, 100)
	if len(got) != 2 {
		t.Fatalf("differently-tagged languages sharing a GR id must stay separate, got %d: %+v", len(got), got)
	}
}

func TestClusterWorksRejectsDirtyISBN(t *testing.T) {
	// OL1W's ISBN resolves to a different author -> guard rejects -> it must NOT
	// merge into the real cluster, and must stay on its title-norm key.
	works := []Work{
		{WorkID: "OL1W", Title: "The Warded Man", FirstPubYear: 2008, ISBNs: []string{"EN1"}},
		{WorkID: "OLbad", Title: "The Painted Man", FirstPubYear: 2008, ISBNs: []string{"DIRTY"}},
	}
	m := &fakeMatcher{byISBN: map[string]Match{
		"EN1":   {Found: true, WorkID: "6589794", Author: "Peter V. Brett"},
		"DIRTY": {Found: true, WorkID: "3360681", Author: "Jane Doe"},
	}}
	got := ClusterWorks(works, "Peter V. Brett", m, nil, 100, 100)
	if len(got) != 2 {
		t.Fatalf("dirty ISBN must not merge -> 2 entries, got %d: %+v", len(got), got)
	}
}

func TestClusterWorksNoISBNSkipsLookup(t *testing.T) {
	m := brettMatcher()
	works := []Work{
		{WorkID: "OLx", Title: "The Warded Man", FirstPubYear: 2008}, // no ISBNs
	}
	ClusterWorks(works, "Peter V. Brett", m, nil, 100, 100)
	if m.calls != 0 {
		t.Errorf("a work with no ISBN should trigger no Goodreads lookup, got %d", m.calls)
	}
}

func TestClusterWorksTitleNormFallback(t *testing.T) {
	// nil matcher -> pass-1 only: English case/box-set handling, no translation merge.
	works := append(brettWorks(),
		Work{WorkID: "OL6W", Title: "The Warded Man (Demon Cycle Box Set)", FirstPubYear: 2012},
		Work{WorkID: "OL7W", Title: "the warded man", FirstPubYear: 2015},
	)
	got := ClusterWorks(works, "Peter V. Brett", nil, nil, 0, 0)
	for _, w := range got {
		if strings.Contains(strings.ToLower(w.Title), "box set") {
			t.Errorf("box set should be dropped: %q", w.Title)
		}
	}
	// Warded/Cycle/hombre/Malowany/Desert all separate (5); case variant merges
	// into Warded Man -> still 5.
	if len(got) != 5 {
		t.Errorf("title-norm only should leave 5 entries, got %d", len(got))
	}
}

func TestClusterWorksRespectsLookupBudget(t *testing.T) {
	m := brettMatcher()
	ClusterWorks(brettWorks(), "Peter V. Brett", m, nil, 2, 2)
	if m.calls != 2 {
		t.Errorf("matcher should be called exactly maxLookups=2 times, got %d", m.calls)
	}
}

func TestClusterWorksPolishSecondPass(t *testing.T) {
	// Two Polish OL works of Demon Cycle book 1 that Goodreads files separately and
	// OL can't link (language:null): a coverless one-volume reissue and the older
	// two-volume "Księga I". The Goodreads pass leaves them apart; the Lubimyczytać
	// pass resolves both to series 1594#1 and collapses them, backfilling the cover.
	works := []Work{
		{WorkID: "OLen", Title: "The Warded Man", FirstPubYear: 2008, CoverURL: "en.jpg", ISBNs: []string{"EN1"}},
		{WorkID: "OLpl1", Title: "Malowany człowiek", FirstPubYear: 2010, CoverURL: "", ISBNs: []string{"PLx"}},
		{WorkID: "OLpl2", Title: "Malowany człowiek: Księga I", FirstPubYear: 2011, CoverURL: ""},
	}
	gr := &fakeMatcher{byISBN: map[string]Match{
		"EN1": {Found: true, WorkID: "6589794", Author: "Peter V. Brett"},
		// The Polish ISBN resolves to Goodreads' *separate* Polish work — GR can't
		// merge it with the other Polish OL record (which has no ISBN at all).
		"PLx": {Found: true, WorkID: "21446513", Author: "Peter V. Brett"},
	}}
	lc := &fakeTitleMatcher{byTitle: map[string]Match{
		"malowany człowiek":           {Found: true, WorkID: "cykl:1594#1", CoverURL: "lc-pl.jpg", Author: "Peter V. Brett"},
		"malowany człowiek: księga i": {Found: true, WorkID: "cykl:1594#1", CoverURL: "lc-pl.jpg", Author: "Peter V. Brett"},
	}}

	got := ClusterWorks(works, "Peter V. Brett", gr, lc, 100, 100)
	if len(got) != 2 {
		t.Fatalf("want 2 clusters (English bk1, merged Polish bk1), got %d: %+v", len(got), got)
	}
	var polish *Work
	for i := range got {
		if strings.Contains(got[i].Title, "Malowany") {
			polish = &got[i]
		}
	}
	if polish == nil {
		t.Fatalf("expected a merged Polish tile: %+v", got)
	}
	if polish.CoverURL != "lc-pl.jpg" {
		t.Errorf("merged Polish tile should backfill the Lubimyczytać cover, got %q", polish.CoverURL)
	}
	// The English tile must be untouched by the Polish pass.
	if lc.calls != 2 {
		t.Errorf("only the 2 Polish-titled survivors should hit Lubimyczytać, got %d calls", lc.calls)
	}
}

func TestClusterWorksLCBudgetPrioritizesCoverless(t *testing.T) {
	// Three Polish works, one already covered. With a budget of 2 the LC pass must
	// spend it on the two coverless works (backfilling their covers), not waste a
	// lookup on the one that already has a cover.
	works := []Work{
		{WorkID: "A", Title: "Człowiek już z okładką", CoverURL: "have.jpg"},
		{WorkID: "B", Title: "Książka bez okładki", CoverURL: ""},
		{WorkID: "C", Title: "Inna książka pusta", CoverURL: ""},
	}
	lc := &fakeTitleMatcher{byTitle: map[string]Match{
		normTitle("Człowiek już z okładką"): {Found: true, WorkID: "lc:1", CoverURL: "lc-a.jpg", Author: "Stanisław Lem"},
		normTitle("Książka bez okładki"):    {Found: true, WorkID: "lc:2", CoverURL: "lc-b.jpg", Author: "Stanisław Lem"},
		normTitle("Inna książka pusta"):     {Found: true, WorkID: "lc:3", CoverURL: "lc-c.jpg", Author: "Stanisław Lem"},
	}}

	got := ClusterWorks(works, "Stanisław Lem", nil, lc, 0, 2)
	if lc.calls != 2 {
		t.Fatalf("budget=2 should mean exactly 2 lookups, got %d", lc.calls)
	}
	byTitle := map[string]Work{}
	for _, w := range got {
		byTitle[w.Title] = w
	}
	if byTitle["Książka bez okładki"].CoverURL != "lc-b.jpg" || byTitle["Inna książka pusta"].CoverURL != "lc-c.jpg" {
		t.Errorf("coverless works should have been backfilled first: %+v", got)
	}
	if byTitle["Człowiek już z okładką"].CoverURL != "have.jpg" {
		t.Errorf("already-covered work should keep its cover (and not consume budget): %+v", byTitle["Człowiek już z okładką"])
	}
}

func TestLooksPolishGate(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  bool
	}{
		{"Malowany człowiek", true}, // ł
		{"Pustynna Włócznia", true}, // łó -> ł
		{"Księga", true},            // ę
		{"The Warded Man", false},
		{"El hombre marcado", false},   // Spanish — GR's job, not LC's
		{"Le Cycle des démons", false}, // French — é not distinctly Polish
	} {
		if got := looksPolish(Work{Title: tc.title}); got != tc.want {
			t.Errorf("looksPolish(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}

func TestNormTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"The Warded Man", "warded man"},
		{"The Warded Man (2009)", "warded man"},
		{"The Warded Man by Peter V. Brett", "warded man"},
		{"A Game of Thrones", "game of thrones"},
		{"The Warded Man — Special Edition", "warded man"},
	} {
		if got := normTitle(tc.in); got != tc.want {
			t.Errorf("normTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
