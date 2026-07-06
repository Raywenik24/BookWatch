package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// --- fixtures (trimmed live Lubimyczytać markup) ---

// lcCycleDetail builds a `book-card__detail--cycle` block as it appears on both
// search-result and bibliography tiles: a /cykl/<id> link whose text carries the
// display position ("tom 1", or "tom 1.1"/"tom 1.2" for a split-volume edition).
// Empty seriesID omits the block entirely (a standalone book).
func lcCycleDetail(seriesID, tomLabel string) string {
	if seriesID == "" {
		return ""
	}
	return fmt.Sprintf(`<div class="book-card__detail book-card__detail--cycle"> Cykl:<a class="book-card__highlighted book-card__highlighted--link" href="/cykl/%s/cykl-demoniczny"> Cykl Demoniczny (tom %s)</a></div>`, seriesID, tomLabel)
}

// lcSearchCard builds one `div.book-card` search-result tile — the real hit
// markup, as opposed to the unrelated `result-tile` promo/recommendation tiles a
// results page leads with. This is what lcSearchHits parses; no /ksiazka page
// fetch happens anymore, so title/cover/author/series all have to come from
// here. seriesID/tomLabel are "" for a standalone (non-series) book.
func lcSearchCard(id, title, cover, author, seriesID, tomLabel string) string {
	return fmt.Sprintf(`<div class="book-card" data-book-id="%[1]s"><div class="book-card__primary"><div class="book-card__cover-wrapper">
<a class="book-card__cover-link" href="/ksiazka/%[1]s/slug"><img class="book-card__cover-image" src="%[3]s" alt="%[2]s" /></a></div>
<div class="book-card__info-box"><a class="book-card__title" href="/ksiazka/%[1]s/slug">%[2]s</a>
<div class="book-card__author"><a href="/autor/1/slug">%[4]s</a></div>%[5]s</div></div></div>`, id, title, cover, author, lcCycleDetail(seriesID, tomLabel))
}

// lcPromoTile builds an unrelated recommendation-carousel tile — different
// markup (`result-tile`, no data-book-id) — that a real results page leads with.
func lcPromoTile(id, title string) string {
	return fmt.Sprintf(`<div class="result-tile result-tile--book"><a class="result-tile__wrapper" href="/ksiazka/%s/promo"><span class="result-tile__title">%s</span></a></div>`, id, title)
}

// lcSearchPage wraps search-result tiles as they sit on a real results page:
// promo tiles first, then the genuine book-card hits.
func lcSearchPage(promos []string, cards []string) string {
	return `<!DOCTYPE html><html><body><div id="ksiazki">` +
		strings.Join(promos, "") + strings.Join(cards, "") +
		`</div></body></html>`
}

// lcAuthorSearchPage builds a /szukaj/autorzy results page linking to authorPath.
func lcAuthorSearchPage(authorPath, name string) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><body>
<a class="newsBoxBook__title newsBoxBook__title--profil" href="%s"><span>%s</span></a>
</body></html>`, authorPath, name)
}

// lcAuthorCard builds one book-card tile as it appears in an author bibliography
// (year + series, unlike a search-result card).
func lcAuthorCard(id, title, cover, year, seriesID, tomLabel string) string {
	return fmt.Sprintf(`<div class="book-card" data-book-id="%[1]s"><div class="book-card__primary"><div class="book-card__cover-wrapper">
<a class="book-card__cover-link" href="/ksiazka/%[1]s/slug"><img class="book-card__cover-image" src="%[3]s" alt="%[2]s" /></a>
<div class="book-card__detail book-card__detail--date"><span class="book-card__highlighted">%[4]s</span></div></div>
<div class="book-card__info-box"><a class="book-card__title" href="/ksiazka/%[1]s/slug">%[2]s</a>
<div class="book-card__author"> Peter V. Brett </div>%[5]s</div></div></div>`, id, title, cover, year, lcCycleDetail(seriesID, tomLabel))
}

func lcAuthorPage(cards ...string) string {
	return `<!DOCTYPE html><html><body><section id="books-and-magazines">` + strings.Join(cards, "") + `</section></body></html>`
}

// lcBookPage builds a /ksiazka detail page as the live site serves it: the full
// blurb sits in #book-description with <br> paragraph breaks, while the JSON-LD
// Book record carries the date + cover but NO description (verified live on
// book 4544). og:description is the shorter fallback.
func lcBookPage(title, desc, cover, date string) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><head>
<meta property="og:description" content="OG %[2]s" />
<meta property="og:image" content="%[3]s" />
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"Book","name":"%[1]s","image":"%[3]s","datePublished":"%[4]s"}
</script></head><body>
<div class="book__description" id="book-description"> %[2]s<br />Druga linia.</div>
<dl><dt>Data wydania:</dt><dd>%[4]s</dd></dl>
</body></html>`, title, desc, cover, date)
}

// newLCTestServer stands up an offline Lubimyczytać mirroring the real shape: a
// results page leads with unrelated promo tiles, then the genuine book-card
// hits (one search request per lookup), plus an author page for the
// bibliography path. Demon Cycle book 1 has two Polish editions — the old
// two-volume "Księga I" split (tom 1.1) and the 2021 one-volume reissue (tom
// 1) — that must merge under the same series+position key; book 2 ("Pustynna
// Włócznia", tom 2) must not.
func newLCTestServer(t *testing.T) (*LCClient, *httptest.Server) {
	t.Helper()
	// phrase (lowercased title) -> search page body
	search := map[string]string{
		"malowany człowiek": lcSearchPage(
			[]string{lcPromoTile("999", "Emisariusz. Powieść z Uniwersum")},
			[]string{lcSearchCard("4983288", "Malowany człowiek", "https://s.lubimyczytac.pl/upload/books/4983288/covB.jpg", "Peter V. Brett", "1594", "1")},
		),
		"malowany człowiek: księga i": lcSearchPage(nil,
			[]string{lcSearchCard("27475", "Malowany człowiek: Księga I", "https://s.lubimyczytac.pl/upload/books/27475/covA.jpg", "Peter V. Brett", "1594", "1.1")},
		),
		"pustynna włócznia": lcSearchPage(nil,
			[]string{lcSearchCard("5001456", "Pustynna Włócznia", "https://s.lubimyczytac.pl/upload/books/5001456/covC.jpg", "Peter V. Brett", "1594", "2")},
		),
		"the painted man": lcSearchPage(nil,
			[]string{lcSearchCard("900001", "The Painted Man", "https://s.lubimyczytac.pl/upload/books/900001/covEN.jpg", "Peter V. Brett", "", "")},
		),
		"obca książka": lcSearchPage(nil,
			[]string{lcSearchCard("900002", "Obca książka", "https://s.lubimyczytac.pl/upload/books/900002/covX.jpg", "Jan Kowalski", "", "")},
		),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case r.URL.Path == "/szukaj/ksiazki":
			w.Write([]byte(search[strings.ToLower(r.URL.Query().Get("phrase"))])) //nolint:errcheck
		case r.URL.Path == "/szukaj/autorzy":
			w.Write([]byte(lcAuthorSearchPage("/autor/18930/peter-v-brett", "Peter V. Brett"))) //nolint:errcheck
		case strings.HasPrefix(r.URL.Path, "/autor/"):
			w.Write([]byte(lcAuthorPage( //nolint:errcheck
				lcAuthorCard("4983288", "Malowany człowiek", "https://s.lubimyczytac.pl/upload/books/4983288/covB.jpg", "2021", "1594", "1"),
				lcAuthorCard("5001456", "Pustynna Włócznia", "https://s.lubimyczytac.pl/upload/books/5001456/covC.jpg", "2022", "1594", "2"),
				lcAuthorCard("700900", "Nowa Nowela", "https://s.lubimyczytac.pl/upload/books/700900/covN.jpg", "2024", "", ""),
			)))
		case strings.HasPrefix(r.URL.Path, "/ksiazka/"):
			w.Write([]byte(lcBookPage( //nolint:errcheck
				"Malowany człowiek", "Polski opis książki.",
				"https://s.lubimyczytac.pl/upload/books/4983288/covB.jpg", "2021-10-29")))
		default:
			http.NotFound(w, r)
		}
	}))
	c := NewLubimyczytac("bookwatch-test/1.0", 5*time.Second)
	c.baseURL = srv.URL
	c.minGap = 0
	return c, srv
}

func TestLCMatchWorkResolvesByTitle(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()

	m := c.MatchWork("Malowany człowiek", "Peter V. Brett", nil)
	if !m.Found {
		t.Fatal("expected a match for the Polish title")
	}
	if m.WorkID != "cykl:1594#1" {
		t.Errorf("work id %q, want cykl:1594#1 (series+position dedup key)", m.WorkID)
	}
	if !strings.Contains(m.CoverURL, "covB.jpg") {
		t.Errorf("cover %q — should come straight from the search tile", m.CoverURL)
	}
	if m.Author != "Peter V. Brett" {
		t.Errorf("author %q", m.Author)
	}
}

func TestLCMatchWorkClustersDifferentlyTitledReissue(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	// The two-volume "Księga I" split and the one-volume reissue are the same
	// original novel — both series 1594, position 1 (the split shows "tom 1.1" but
	// the truncated leading integer recovers the true position) — so they must
	// merge under the same work id, restored even with the 1-request shortcut.
	old := c.MatchWork("Malowany człowiek: Księga I", "Peter V. Brett", nil)
	reissue := c.MatchWork("Malowany człowiek", "Peter V. Brett", nil)
	if !old.Found || !reissue.Found {
		t.Fatalf("both editions should resolve: old=%+v reissue=%+v", old, reissue)
	}
	if old.WorkID != reissue.WorkID {
		t.Errorf("editions of the same volume must share a work id: %q vs %q", old.WorkID, reissue.WorkID)
	}
	// A different volume (book 2, position 2) must NOT share it.
	spear := c.MatchWork("Pustynna Włócznia", "Peter V. Brett", nil)
	if spear.WorkID == reissue.WorkID {
		t.Errorf("book 2 must be a distinct work, got %q", spear.WorkID)
	}
}

func TestLCMatchWorkOneRequest(t *testing.T) {
	// The whole point of the shortcut: a match must cost exactly one HTTP request
	// (the search), never a follow-up /ksiazka fetch.
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(lcSearchPage(nil, //nolint:errcheck
			[]string{lcSearchCard("4983288", "Malowany człowiek", "c.jpg", "Peter V. Brett", "", "")})))
	}))
	defer srv.Close()
	c := NewLubimyczytac("t", 5*time.Second)
	c.baseURL = srv.URL
	c.minGap = 0

	m := c.MatchWork("Malowany człowiek", "Peter V. Brett", nil)
	if !m.Found {
		t.Fatal("expected a match")
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 request, got %d", hits)
	}
}

func TestLCMatchWorkIgnoresUnrelatedPromoTiles(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	// The search response for "Malowany człowiek" leads with an unrelated promo
	// tile (Emisariusz); it must never be picked over the real book-card hit.
	m := c.MatchWork("Malowany człowiek", "Peter V. Brett", nil)
	if m.WorkID == "lc:999" {
		t.Errorf("promo/recommendation tile leaked into the match: %+v", m)
	}
}

func TestLCMatchWorkRejectsWrongAuthor(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	// The title resolves to a book by a different author — the guard must reject it.
	if m := c.MatchWork("Obca książka", "Peter V. Brett", nil); m.Found {
		t.Errorf("wrong-author hit must be rejected, got work %q", m.WorkID)
	}
}

func TestLCMatchWorkCache(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(lcSearchPage(nil, //nolint:errcheck
			[]string{lcSearchCard("4983288", "Malowany człowiek", "c.jpg", "Peter V. Brett", "", "")})))
	}))
	defer srv.Close()
	c := NewLubimyczytac("t", 5*time.Second)
	c.baseURL = srv.URL
	c.minGap = 0

	c.MatchWork("Malowany człowiek", "Peter V. Brett", nil)
	first := hits
	c.MatchWork("Malowany człowiek", "Peter V. Brett", nil)
	if hits != first {
		t.Errorf("second lookup of the same title should be cached: %d extra request(s)", hits-first)
	}
}

func TestLCMiss(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	if m := c.MatchWork("Nieistniejący Tytuł", "Nikt", nil); m.Found {
		t.Error("a search with no book hit must miss")
	}
	if m := c.MatchWork("", "Anyone", nil); m.Found {
		t.Error("an empty title must miss without a request")
	}
}

func TestLCServerErrorIsAMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewLubimyczytac("t", 3*time.Second)
	c.baseURL = srv.URL
	c.minGap = 0
	if m := c.MatchWork("Malowany człowiek", "Peter V. Brett", nil); m.Found {
		t.Error("a 503 must read as a miss, not a crash")
	}
}

func TestLCAuthorSearch(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	if got := c.AuthorSearch("Peter V. Brett"); got != "/autor/18930/peter-v-brett" {
		t.Errorf("AuthorSearch = %q, want /autor/18930/peter-v-brett", got)
	}
}

func TestLCAuthorWorks(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	works, err := c.AuthorWorks("/autor/18930/peter-v-brett")
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 3 {
		t.Fatalf("want 3 bibliography works, got %d: %+v", len(works), works)
	}
	first := works[0]
	if first.Title != "Malowany człowiek" || first.FirstPubYear != 2021 {
		t.Errorf("first work parsed wrong: %q %d", first.Title, first.FirstPubYear)
	}
	if first.WorkID != "cykl:1594#1" || !strings.Contains(first.CoverURL, "covB.jpg") {
		t.Errorf("first work id/cover wrong: %q %q", first.WorkID, first.CoverURL)
	}
	// Book 2 of the same series must NOT collide with book 1's work id — a bare
	// series id (no position) would wrongly treat every volume as one identity.
	second := works[1]
	if second.WorkID != "cykl:1594#2" || second.WorkID == first.WorkID {
		t.Errorf("book 2 must have a distinct work id from book 1, got %q vs %q", second.WorkID, first.WorkID)
	}
	// The standalone novel with no series falls back to the LC book id.
	if last := works[2]; last.WorkID != "lc:700900" {
		t.Errorf("seriesless work should key on lc:<bookid>, got %q", last.WorkID)
	}
}

func TestLCSearchHitsRanksByTitle(t *testing.T) {
	// A real results page leads with unrelated promo/recommendation tiles using a
	// different markup (result-tile, no data-book-id); scoping to book-card alone
	// already excludes them, and ranking picks the best of several real hits.
	page := lcSearchPage(
		[]string{lcPromoTile("999", "Emisariusz. Powieść z Uniwersum")},
		[]string{
			lcSearchCard("4983288", "Malowany człowiek", "b.jpg", "Peter V. Brett", "", ""),
			lcSearchCard("27475", "Malowany człowiek: Księga I", "a.jpg", "Peter V. Brett", "", ""),
		},
	)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	got := lcSearchHits(doc, "Malowany człowiek")
	if len(got) != 2 {
		t.Fatalf("want 2 title-matching hits (promo excluded), got %d: %+v", len(got), got)
	}
	for _, h := range got {
		if !strings.Contains(h.Title, "Malowany") {
			t.Errorf("unrelated promo leaked into hits: %+v", h)
		}
	}
	// Exact title match should score at least as high as the longer variant.
	if got[0].WorkID != "lc:4983288" {
		t.Errorf("best hit should be the exact title match, got %+v", got[0])
	}
}

func TestLCSeriesKeyTruncatesSplitVolumePosition(t *testing.T) {
	page := `<!DOCTYPE html><html><body>` +
		lcSearchCard("27475", "X", "c.jpg", "Y", "1594", "1.1") +
		lcSearchCard("4983288", "X", "c.jpg", "Y", "1594", "1") +
		`</body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	doc.Find("div.book-card").Each(func(_ int, card *goquery.Selection) {
		keys = append(keys, lcSeriesKey(card))
	})
	if len(keys) != 2 || keys[0] != "cykl:1594#1" || keys[1] != "cykl:1594#1" {
		t.Errorf("split (tom 1.1) and combined (tom 1) editions must both key to cykl:1594#1, got %v", keys)
	}
}

func TestLCFoldMatchesSlug(t *testing.T) {
	if lcFold("Pustynna Włócznia") != "pustynna wlocznia" {
		t.Errorf("fold = %q", lcFold("Pustynna Włócznia"))
	}
	tokens := lcTokens("Malowany człowiek: Księga I")
	for _, tok := range []string{"malowany", "czlowiek", "ksiega"} {
		if !tokens[tok] {
			t.Errorf("token %q missing from %v", tok, tokens)
		}
	}
}

// sanity: the search phrase is URL-escaped, so diacritics survive the round trip.
func TestLCSearchEscapesPhrase(t *testing.T) {
	if q := url.QueryEscape("Malowany człowiek"); !strings.Contains(q, "cz") {
		t.Errorf("unexpected escaping: %q", q)
	}
}

func TestLCSearchCandidates(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()

	got := c.SearchCandidates("Malowany człowiek")
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	r := got[0]
	// The bare book id (not the series-position key) is the note's Work ID, and
	// the /ksiazka page URL is the Link/dedup key.
	if r.BookID != "4983288" {
		t.Errorf("book id %q, want 4983288 (bare id, not series key)", r.BookID)
	}
	if !strings.HasSuffix(r.URL, "/ksiazka/4983288/slug") {
		t.Errorf("url %q, want absolute /ksiazka/4983288/slug", r.URL)
	}
	if r.Title != "Malowany człowiek" || r.Author != "Peter V. Brett" {
		t.Errorf("title/author parsed wrong: %q / %q", r.Title, r.Author)
	}
	if !strings.Contains(r.CoverURL, "covB.jpg") {
		t.Errorf("cover %q — should come from the search tile", r.CoverURL)
	}
}

func TestLCSearchCandidatesKeepsWrongAuthorHits(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	// Unlike the clustering match (which guards on author), the picker fallback
	// surfaces every hit and lets the user disambiguate.
	got := c.SearchCandidates("Obca książka")
	if len(got) != 1 || got[0].Author != "Jan Kowalski" {
		t.Errorf("want the by-a-different-author hit surfaced, got %+v", got)
	}
}

func TestLCSearchCandidatesEmptyTitle(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	if got := c.SearchCandidates("   "); got != nil {
		t.Errorf("blank title should yield no candidates without a request, got %+v", got)
	}
}

func TestLCBookDetail(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()

	// The detail fetch takes the book's full /ksiazka page URL (bare /ksiazka/<id>
	// 404s on the live site), so pass the slugged URL the search result carries.
	b, err := c.BookDetail(srv.URL + "/ksiazka/4983288/slug")
	if err != nil {
		t.Fatal(err)
	}
	// The full blurb comes from #book-description with its <br> preserved as a
	// newline — not the shorter JSON-LD/OG text.
	if b.Description != "Polski opis książki.\nDruga linia." {
		t.Errorf("description %q — should be the full #book-description blurb", b.Description)
	}
	if b.ReleaseDate != "2021-10-29" {
		t.Errorf("release date %q, want the Polish datePublished 2021-10-29", b.ReleaseDate)
	}
	if !strings.Contains(b.CoverURL, "covB.jpg") {
		t.Errorf("cover %q", b.CoverURL)
	}
}

func TestLCBookDetailEmptyURL(t *testing.T) {
	c, srv := newLCTestServer(t)
	defer srv.Close()
	if _, err := c.BookDetail("  "); err == nil {
		t.Error("an empty book url must error, not fetch")
	}
}

func TestLCRelPath(t *testing.T) {
	cases := map[string]string{
		"https://lubimyczytac.pl/ksiazka/4544/prawa-i-powinnosci": "/ksiazka/4544/prawa-i-powinnosci",
		"/ksiazka/4544/prawa-i-powinnosci":                        "/ksiazka/4544/prawa-i-powinnosci",
		// A client-supplied foreign host is reduced to its path — fetch always
		// targets the client's own baseURL, so this can't be pointed elsewhere.
		"http://evil.example/ksiazka/1/x": "/ksiazka/1/x",
		"":                                "",
	}
	for in, want := range cases {
		if got := lcRelPath(in); got != want {
			t.Errorf("lcRelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLCBookPrefersBookDescription(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(
		lcBookPage("Tytuł", "Pełny opis.", "https://s.lubimyczytac.pl/c.jpg", "2020")))
	if err != nil {
		t.Fatal(err)
	}
	b := parseLCBook(doc)
	if b.Description != "Pełny opis.\nDruga linia." || b.ReleaseDate != "2020" {
		t.Errorf("#book-description + JSON-LD date expected: %+v", b)
	}
}

func TestParseLCBookFallsBackToOG(t *testing.T) {
	// No #book-description and no JSON-LD — the blurb, cover and date must come
	// from OG meta + the "Data wydania" detail row instead.
	page := `<!DOCTYPE html><html><head>
<meta property="og:description" content="Blurb z Open Graph." />
<meta property="og:image" content="https://s.lubimyczytac.pl/og.jpg" />
</head><body><dl><dt>Data wydania:</dt><dd>2019-05-01</dd></dl></body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	b := parseLCBook(doc)
	if b.Description != "Blurb z Open Graph." {
		t.Errorf("description fallback %q", b.Description)
	}
	if b.CoverURL != "https://s.lubimyczytac.pl/og.jpg" {
		t.Errorf("cover fallback %q", b.CoverURL)
	}
	if b.ReleaseDate != "2019-05-01" {
		t.Errorf("date fallback %q", b.ReleaseDate)
	}
}

func TestParseLCJSONLDImageVariants(t *testing.T) {
	cases := map[string]string{
		`{"@type":"Book","image":"a.jpg"}`:                     "a.jpg",
		`{"@type":"Book","image":["b.jpg","c.jpg"]}`:           "b.jpg",
		`{"@type":"Book","image":{"url":"d.jpg"}}`:             "d.jpg",
		`{"@type":["Product","Book"],"image":{"url":"e.jpg"}}`: "e.jpg",
	}
	for raw, want := range cases {
		ld, ok := parseLCJSONLD(raw)
		if !ok {
			t.Errorf("%s: not recognized as a Book", raw)
			continue
		}
		if got := ld.imageURL(); got != want {
			t.Errorf("%s: image %q, want %q", raw, got, want)
		}
	}
	// An @graph-wrapped Book must be found too.
	if _, ok := parseLCJSONLD(`{"@graph":[{"@type":"WebPage"},{"@type":"Book","description":"x"}]}`); !ok {
		t.Error("@graph-wrapped Book not found")
	}
}
