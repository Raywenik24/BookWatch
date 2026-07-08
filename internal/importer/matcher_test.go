package importer

import (
	"errors"
	"strings"
	"testing"

	"bookwatch/internal/calibre"
	"bookwatch/internal/provider"
	"bookwatch/internal/scraper"
)

// --- stubs ---

type stubOL struct {
	byISBN    map[string]provider.Candidate // isbn (raw) -> candidate
	byTitle   map[string][]provider.Candidate
	isbnErr   error
	titleErr  error
	isbnCalls int
}

func (s *stubOL) ByISBN(isbn string) (provider.Candidate, error) {
	s.isbnCalls++
	if s.isbnErr != nil {
		return provider.Candidate{}, s.isbnErr
	}
	return s.byISBN[isbn], nil
}

func (s *stubOL) SearchByTitle(q string) ([]provider.Candidate, error) {
	if s.titleErr != nil {
		return nil, s.titleErr
	}
	return s.byTitle[q], nil
}

type stubLC struct{ byTitle map[string][]provider.LCSearchResult }

func (s *stubLC) SearchCandidates(title string) []provider.LCSearchResult {
	return s.byTitle[title]
}

type stubLN struct {
	byQuery map[string][]scraper.SearchResult
	err     error
}

func (s *stubLN) SearchTitle(q string) ([]scraper.SearchResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.byQuery[q], nil
}

// --- classification ---

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		book calibre.Book
		want Kind
	}{
		{"ln volume (tag + series)", calibre.Book{Tags: []string{"Light Novel"}, Series: "Overlord"}, KindLNVolume},
		{"standalone ln (tag, no series)", calibre.Book{Tags: []string{"light novel"}}, KindLNSeries},
		{"polish by lang", calibre.Book{Languages: []string{"pol"}}, KindPolish},
		{"polish by identifier", calibre.Book{Languages: []string{"eng"}, Identifiers: map[string]string{"lubimyczytac": "1"}}, KindPolish},
		{"english default", calibre.Book{Languages: []string{"eng"}}, KindEnglish},
		{"dual-language stays english", calibre.Book{Languages: []string{"eng", "pol"}}, KindEnglish},
	}
	for _, c := range cases {
		if got := classify(c.book); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- English backend ---

func TestMatchEnglishISBN(t *testing.T) {
	ol := &stubOL{byISBN: map[string]provider.Candidate{
		"9781234567890": {WorkID: "OL1W", OLURL: "https://openlibrary.org/works/OL1W"},
	}}
	m := New(ol, nil, nil, 0)
	r := m.Match(calibre.Book{
		Title:       "The Warded Man",
		Languages:   []string{"eng"},
		Identifiers: map[string]string{"isbn": "9781234567890"},
	})
	if r.Kind != KindEnglish || r.Confidence != Confident {
		t.Fatalf("want confident english, got %+v", r)
	}
	if r.WorkID != "OL1W" || r.ResolvedLink != "https://openlibrary.org/works/OL1W" {
		t.Errorf("resolved wrong: %+v", r)
	}
	if r.Unmatched {
		t.Error("ISBN-exact match should not be unmatched")
	}
}

func TestMatchEnglishTitleFallbackConfident(t *testing.T) {
	ol := &stubOL{
		byISBN: map[string]provider.Candidate{}, // ISBN present but no hit → falls through
		byTitle: map[string][]provider.Candidate{
			"The Warded Man": {
				{Title: "The Warded Man", Author: "Peter V. Brett", WorkID: "OL9W", OLURL: "u9"},
				{Title: "Some Other Book", Author: "Nobody Else", WorkID: "OLxW", OLURL: "ux"},
			},
		},
	}
	m := New(ol, nil, nil, 0)
	r := m.Match(calibre.Book{
		Title:       "The Warded Man",
		Authors:     []string{"Peter V. Brett"},
		Languages:   []string{"eng"},
		Identifiers: map[string]string{"isbn": "0000000000000"},
	})
	if ol.isbnCalls != 1 {
		t.Errorf("ISBN should have been tried once, got %d calls", ol.isbnCalls)
	}
	if r.Confidence != Confident || r.WorkID != "OL9W" {
		t.Fatalf("want confident title match OL9W, got %+v", r)
	}
}

func TestMatchEnglishUncertainSeveralPlausible(t *testing.T) {
	// Two exact-title, author-matching editions filed as separate works → ambiguous.
	ol := &stubOL{byTitle: map[string][]provider.Candidate{
		"Dune": {
			{Title: "Dune", Author: "Frank Herbert", WorkID: "OLaW", OLURL: "ua"},
			{Title: "Dune", Author: "Frank Herbert", WorkID: "OLbW", OLURL: "ub"},
		},
	}}
	m := New(ol, nil, nil, 0)
	r := m.Match(calibre.Book{Title: "Dune", Authors: []string{"Frank Herbert"}, Languages: []string{"eng"}})
	if r.Confidence != Uncertain || !r.Unmatched {
		t.Fatalf("want uncertain/unmatched, got %+v", r)
	}
	if len(r.Candidates) != 2 {
		t.Errorf("want 2 fallback candidates, got %d", len(r.Candidates))
	}
	if !isSynthetic(r.ResolvedLink) {
		t.Errorf("uncertain match should carry a synthetic link, got %q", r.ResolvedLink)
	}
}

func TestMatchEnglishUncertainNoAuthorMatch(t *testing.T) {
	ol := &stubOL{byTitle: map[string][]provider.Candidate{
		"Common Title": {{Title: "Common Title", Author: "Wrong Person", WorkID: "OLzW", OLURL: "uz"}},
	}}
	m := New(ol, nil, nil, 0)
	r := m.Match(calibre.Book{Title: "Common Title", Authors: []string{"Real Author"}, Languages: []string{"eng"}})
	if r.Confidence != Uncertain || r.WorkID != "" {
		t.Fatalf("want uncertain with no workID, got %+v", r)
	}
	if len(r.Candidates) != 1 {
		t.Errorf("want the one hit kept as a fallback, got %d", len(r.Candidates))
	}
}

func TestMatchEnglishNetworkErrorDistinct(t *testing.T) {
	boom := errors.New("network down")
	ol := &stubOL{titleErr: boom}
	m := New(ol, nil, nil, 0)
	r := m.Match(calibre.Book{Title: "Whatever", Languages: []string{"eng"}})
	if r.Err == nil {
		t.Fatal("network failure must set Err")
	}
	if r.Unmatched || r.Confidence == Confident || r.ResolvedLink != "" {
		t.Errorf("an errored item is not an unmatched item: %+v", r)
	}
}

// --- Polish backend ---

func TestMatchPolishConfident(t *testing.T) {
	lc := &stubLC{byTitle: map[string][]provider.LCSearchResult{
		"Wiedźmin": {
			{BookID: "42", URL: "https://lubimyczytac.pl/ksiazka/42/wiedzmin", Title: "Wiedźmin", Author: "Andrzej Sapkowski"},
		},
	}}
	m := New(nil, lc, nil, 0)
	r := m.Match(calibre.Book{Title: "Wiedźmin", Authors: []string{"Andrzej Sapkowski"}, Languages: []string{"pol"}})
	if r.Kind != KindPolish || r.Confidence != Confident {
		t.Fatalf("want confident polish, got %+v", r)
	}
	if r.WorkID != "42" || r.ResolvedLink != "https://lubimyczytac.pl/ksiazka/42/wiedzmin" {
		t.Errorf("resolved wrong: %+v", r)
	}
}

func TestMatchPolishUncertainNoHits(t *testing.T) {
	m := New(nil, &stubLC{}, nil, 0)
	r := m.Match(calibre.Book{Title: "Nieznana Książka", Languages: []string{"pol"}})
	if r.Confidence != Uncertain || !r.Unmatched || len(r.Candidates) != 0 {
		t.Fatalf("want empty uncertain result, got %+v", r)
	}
	if !isSynthetic(r.ResolvedLink) {
		t.Errorf("want synthetic link, got %q", r.ResolvedLink)
	}
}

// --- LN backend ---

func TestMatchLNVolumeNotMatched(t *testing.T) {
	m := New(nil, nil, nil, 0)
	r := m.Match(calibre.Book{Title: "Overlord Volume 3", Tags: []string{"Light Novel"}, Series: "Overlord"})
	if r.Kind != KindLNVolume {
		t.Fatalf("want KindLNVolume, got %v", r.Kind)
	}
	if r.ResolvedLink != "" || r.Confidence != "" || r.Unmatched {
		t.Errorf("an LN volume is untouched by matching: %+v", r)
	}
}

func TestMatchSeriesConfident(t *testing.T) {
	ln := &stubLN{byQuery: map[string][]scraper.SearchResult{
		"Overlord": {
			{Title: "Overlord Light Novel Epub", URL: "https://jnovels.com/overlord/"},
			{Title: "Overlord Volume 1 Epub", URL: "https://jnovels.com/overlord-v1/"},
		},
	}}
	m := New(nil, nil, ln, 0)
	r := m.MatchSeries("Overlord", "")
	if r.Kind != KindLNSeries || r.Confidence != Confident {
		t.Fatalf("want confident ln series, got %+v", r)
	}
	if r.ResolvedLink != "https://jnovels.com/overlord/" {
		t.Errorf("resolved wrong link: %q", r.ResolvedLink)
	}
	if r.WorkID != "" {
		t.Errorf("jnovels has no work id, got %q", r.WorkID)
	}
}

func TestMatchSeriesUncertainKeepsCandidates(t *testing.T) {
	// The series isn't really on jnovels — search returns only tangential posts
	// that cover well under two-thirds of the query tokens → uncertain.
	ln := &stubLN{byQuery: map[string][]scraper.SearchResult{
		"That Time I Reincarnated Slime": {
			{Title: "Slime Diaries Epub", URL: "u1"},
			{Title: "Unrelated Isekai Epub", URL: "u2"},
			{Title: "Another Novel Epub", URL: "u3"},
			{Title: "Fourth Post Epub", URL: "u4"},
		},
	}}
	m := New(nil, nil, ln, 0)
	r := m.MatchSeries("That Time I Reincarnated Slime", "")
	if r.Confidence != Uncertain || !r.Unmatched {
		t.Fatalf("want uncertain, got %+v", r)
	}
	if len(r.Candidates) != maxFallbacks {
		t.Errorf("want %d fallbacks, got %d", maxFallbacks, len(r.Candidates))
	}
}

func TestMatchSeriesNetworkError(t *testing.T) {
	m := New(nil, nil, &stubLN{err: errors.New("timeout")}, 0)
	r := m.MatchSeries("Anything", "")
	if r.Err == nil || r.Unmatched {
		t.Fatalf("network error must be distinct from unmatched: %+v", r)
	}
}

func TestStandaloneLNMatchedByTitle(t *testing.T) {
	ln := &stubLN{byQuery: map[string][]scraper.SearchResult{
		"Solo Novel": {{Title: "Solo Novel Light Novel Epub", URL: "https://jnovels.com/solo/"}},
	}}
	m := New(nil, nil, ln, 0)
	r := m.Match(calibre.Book{Title: "Solo Novel", Tags: []string{"Light Novel"}}) // no series
	if r.Kind != KindLNSeries || r.Confidence != Confident || r.ResolvedLink != "https://jnovels.com/solo/" {
		t.Fatalf("standalone LN should match by title: %+v", r)
	}
}

// --- nil backends degrade gracefully ---

func TestNilBackendUnmatched(t *testing.T) {
	m := New(nil, nil, nil, 0)
	r := m.Match(calibre.Book{Title: "Orphan", Languages: []string{"eng"}})
	if r.Confidence != Uncertain || !r.Unmatched || r.Err != nil {
		t.Fatalf("nil backend should yield a clean unmatched result, got %+v", r)
	}
}

// --- helpers ---

func TestSyntheticLink(t *testing.T) {
	if got := syntheticLink("The Warded Man!"); got != "https://unmatched.bookwatch.invalid/the-warded-man" {
		t.Errorf("syntheticLink = %q", got)
	}
	if got := syntheticLink("   "); got != "https://unmatched.bookwatch.invalid/unknown" {
		t.Errorf("blank title slug = %q", got)
	}
}

func isSynthetic(link string) bool {
	return strings.HasPrefix(link, "https://unmatched.bookwatch.invalid/")
}
