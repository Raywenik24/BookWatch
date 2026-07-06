package provider

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bookwatch/internal/scraper"

	"github.com/PuerkitoBio/goquery"
)

const (
	lcBaseURL = "https://lubimyczytac.pl"
	lcMaxBody = 8 << 20 // 8 MiB — an author/book page is ~200–400 KiB
	lcMinGap  = 700 * time.Millisecond
)

// LCClient resolves a Polish OpenLibrary work to its Lubimyczytać cluster so the
// baseline picker can collapse Polish edition dupes and backfill Polish covers
// that Goodreads can't (#43). It is the Polish-language counterpart to GRClient
// and composes after it as a second pass in ClusterWorks.
//
// Unlike Goodreads, Lubimyczytać has no per-ISBN entry point and its ISBN search
// returns generic noise (verified live before building — the same "check the
// entry point first" lesson as the Goodreads /search WAF block). What works:
//
//   - /szukaj/ksiazki?phrase=<title> server-renders real hits as `div.book-card`
//     tiles under #ksiazki — each already carrying the title, cover and author,
//     so no second fetch to a /ksiazka page is needed. (The page also leads with
//     unrelated promo/recommendation tiles using a *different* markup,
//     `result-tile`; scoping to `.book-card` already excludes them.)
//
// So a match is ONE request: search the title, rank the book-card hits by title
// overlap, take the best one whose author matches. This used to be two requests
// (search + a /ksiazka page fetch for JSON-LD) which made a Polish author's
// clustering pass take minutes — the first live smoke test caught it.
//
// The book-card tile also carries the same series/cycle detail the /ksiazka page
// did, so the cross-title merge (an old two-volume "Księga I" split vs. a new
// one-volume release, both the same original volume) still works from the tile
// alone: dedup key = series id + position (lcSeriesKey). One wrinkle, confirmed
// live — a split-volume edition's displayed position has a sub-number ("tom
// 1.1", "tom 1.2") while the combined edition just shows "tom 1"; the book
// page's actual position field is "1" for both, so lcSeriesKey keeps only the
// leading integer ("1.1" -> "1") to recover it without the extra fetch.
//
// Beyond clustering, the same search+book-page plumbing backs the add-a-book
// Polish fallback (#60): SearchCandidates surfaces every hit for the picker when
// OpenLibrary has nothing, and BookDetail lazy-fetches the picked book's
// /ksiazka page for its blurb + Polish publication date.
//
// Every request is rate-limited (~700ms), sent through the SSRF-guarded shared
// client, and memoized per normalized title. A miss or outage degrades to the
// Goodreads/OpenLibrary result — this is a supplement, never a replacement.
// There is no page-level language signal on a search hit, so this relies on the
// caller (ClusterWorks' looksPolish gate) to only route plausibly-Polish titles
// here, plus the author-match guard to reject a wrong-book hit.
type LCClient struct {
	baseURL   string
	http      *http.Client
	userAgent string
	minGap    time.Duration

	mu       sync.Mutex
	lastReq  time.Time
	cache    map[string]Match // key: normalized title
	warnOnce sync.Once
}

// PolishSource surfaces an author's Polish bibliography from Lubimyczytać, for
// the tracker poll's Polish-release pass (#43), and resolves a single title to
// its Polish edition for the opt-in translation-watch pass (#46). *LCClient
// implements it; the poll accepts a nil source (Polish passes disabled) and
// injects a fake in tests.
type PolishSource interface {
	// AuthorSearch resolves an author name to their Lubimyczytać author path, or "".
	AuthorSearch(name string) string
	// AuthorWorks returns the author's bibliography as catalog Works.
	AuthorWorks(authorPath string) ([]Work, error)
	// MatchWork resolves one title+author to its Lubimyczytać edition, if any.
	MatchWork(title, author string, isbns []string) Match
}

// NewLubimyczytac creates a Lubimyczytać title-resolution client. A blank
// userAgent falls back to a browser-like string (the pages serve any UA, but a
// sensible one is polite).
func NewLubimyczytac(userAgent string, timeout time.Duration) *LCClient {
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (BookWatch/1.0; +https://github.com/Raywenik24/BookWatch)"
	}
	return &LCClient{
		baseURL:   lcBaseURL,
		http:      scraper.NewGuardedHTTPClient(timeout),
		userAgent: userAgent,
		minGap:    lcMinGap,
		cache:     map[string]Match{},
	}
}

// MatchWork resolves a Polish-titled work to its Lubimyczytać cluster by title.
// The isbns argument is ignored — Lubimyczytać has no usable ISBN lookup — so
// this trusts the caller (ClusterWorks) to only route plausibly-Polish titles
// here. Per-title results are memoized. Never errors — a failure is a !Found
// miss. Implements Matcher.
func (c *LCClient) MatchWork(title, author string, _ []string) Match {
	key := normTitle(title)
	if key == "" {
		return Match{}
	}
	c.mu.Lock()
	if m, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return m
	}
	c.mu.Unlock()

	m := c.resolveByTitle(title, author)

	c.mu.Lock()
	c.cache[key] = m
	c.mu.Unlock()
	return m
}

// lcMaxCandidates caps how many ranked search hits resolveByTitle checks against
// the author guard. This costs nothing extra over the wire — every hit already
// came from the one search page fetched — it only bounds the (rare) page with
// many same-titled hits by different authors.
const lcMaxCandidates = 5

// resolveByTitle runs one search request and returns the first ranked hit whose
// author matches (the wrong-book guard). Candidates are ranked by how many title
// tokens they share with the query, best first — see lcSearchHits.
func (c *LCClient) resolveByTitle(title, author string) Match {
	doc, err := c.fetch("/szukaj/ksiazki?phrase=" + url.QueryEscape(title))
	if err != nil {
		c.warn(err)
		return Match{}
	}
	for i, hit := range lcSearchHits(doc, title) {
		if i >= lcMaxCandidates {
			break
		}
		if author != "" && hit.Author != "" && !sameAuthor(author, hit.Author) {
			continue // search landed on a different author's book
		}
		return Match{WorkID: hit.WorkID, Title: hit.Title, CoverURL: hit.CoverURL, Author: hit.Author, Found: true}
	}
	return Match{}
}

// --- add-a-book fallback: multi-candidate search + book-page detail (#60) ---

// LCSearchResult is one Lubimyczytać search hit surfaced to the add-a-book
// picker as a pickable candidate when OpenLibrary has nothing (issue #60). It
// mirrors provider.Candidate's fields the picker renders, but carries LC
// identity — the bare book id (the note's Work ID) and the /ksiazka page URL
// (the note's Link and duplicate-detection key) — rather than the series-
// position dedup key resolveByTitle returns for edition clustering.
type LCSearchResult struct {
	BookID   string `json:"book_id"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Author   string `json:"author"`
	CoverURL string `json:"cover_url"`
}

// lcMaxSearchResults caps how many ranked hits the picker is shown — the whole
// results page was already fetched in one request, so this only bounds how long
// a candidate list gets for a noisy title.
const lcMaxSearchResults = 20

// SearchCandidates runs one search request and returns every book-card hit as a
// pickable candidate, ranked by title-token overlap (best first). It is the
// multi-candidate counterpart to resolveByTitle, which returns only the single
// best author-matched hit for edition clustering; the add-a-book fallback shows
// them all so the user picks (issue #60). No author guard is applied — the user
// disambiguates. Never errors: a failure or outage yields no candidates.
func (c *LCClient) SearchCandidates(title string) []LCSearchResult {
	if strings.TrimSpace(title) == "" {
		return nil
	}
	doc, err := c.fetch("/szukaj/ksiazki?phrase=" + url.QueryEscape(title))
	if err != nil {
		c.warn(err)
		return nil
	}
	hits := lcSearchHits(doc, title)
	out := make([]LCSearchResult, 0, len(hits))
	for i, h := range hits {
		if i >= lcMaxSearchResults {
			break
		}
		if h.BookID == "" {
			continue
		}
		out = append(out, LCSearchResult{
			BookID:   h.BookID,
			URL:      c.absURL(h.Href, h.BookID),
			Title:    h.Title,
			Author:   h.Author,
			CoverURL: h.CoverURL,
		})
	}
	return out
}

// absURL turns a book-card's (relative) /ksiazka href into the canonical page
// URL used as the note's Link. A blank href degrades to /ksiazka/<id>, which the
// site redirects to the slugged URL.
func (c *LCClient) absURL(href, id string) string {
	if href == "" {
		href = "/ksiazka/" + id
	}
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	return c.baseURL + href
}

// LCBook is a Lubimyczytać /ksiazka page's detail — the blurb, Polish
// publication date and cover the search tile doesn't carry, fetched lazily when
// a candidate is picked to add (issue #60). Any field may be empty; the add
// path treats a miss as "no description / no date".
type LCBook struct {
	Description string
	ReleaseDate string
	CoverURL    string
}

// BookDetail fetches a book's /ksiazka page (one request, mirroring the OL
// work-detail path) and reads its blurb + Polish publication date + cover.
// pageURL is the book's own /ksiazka URL from the search result — the bare
// /ksiazka/<id> without the slug 404s, so the full slugged path is required.
// Only its path is used (host is always the client's own baseURL), so a
// client-supplied URL can't be pointed at another host.
func (c *LCClient) BookDetail(pageURL string) (LCBook, error) {
	path := lcRelPath(pageURL)
	if path == "" {
		return LCBook{}, fmt.Errorf("lubimyczytac: empty book url")
	}
	doc, err := c.fetch(path)
	if err != nil {
		return LCBook{}, err
	}
	return parseLCBook(doc), nil
}

// lcRelPath reduces a book URL to the path (+query) fetch() prepends baseURL to.
// A path is returned unchanged; an absolute URL is stripped to its path, so the
// fetch always targets the client's own baseURL host regardless of the input.
func lcRelPath(pageURL string) string {
	pageURL = strings.TrimSpace(pageURL)
	if pageURL == "" {
		return ""
	}
	if strings.HasPrefix(pageURL, "/") {
		return pageURL
	}
	u, err := url.Parse(pageURL)
	if err != nil || u.Path == "" {
		return ""
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// parseLCBook reads a /ksiazka page's detail. The blurb comes from the
// #book-description element (the full text — the page's JSON-LD Book record
// carries no description); the Polish publication date + cover come from that
// JSON-LD record. Each has a fallback (Open Graph meta, the "Data wydania" row)
// so a layout change on one path still yields usable data.
func parseLCBook(doc *goquery.Document) LCBook {
	var b LCBook
	doc.Find(`script[type="application/ld+json"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		ld, ok := parseLCJSONLD(s.Text())
		if !ok {
			return true // not the Book node — keep scanning
		}
		b.Description = strings.TrimSpace(ld.Description)
		b.ReleaseDate = strings.TrimSpace(ld.DatePublished)
		b.CoverURL = strings.TrimSpace(ld.imageURL())
		return false
	})
	if d := lcDescription(doc); d != "" {
		b.Description = d // the full blurb — beats JSON-LD, which carries none
	}
	if b.Description == "" {
		if og, ok := doc.Find(`meta[property="og:description"]`).Attr("content"); ok {
			b.Description = strings.TrimSpace(og)
		}
	}
	if b.CoverURL == "" {
		if og, ok := doc.Find(`meta[property="og:image"]`).Attr("content"); ok {
			b.CoverURL = strings.TrimSpace(og)
		}
	}
	if b.ReleaseDate == "" {
		b.ReleaseDate = lcDetailDate(doc)
	}
	return b
}

var (
	lcBrRE  = regexp.MustCompile(`(?i)<br\s*/?>`)
	lcTagRE = regexp.MustCompile(`<[^>]+>`)
)

// lcDescription reads the full book blurb from the #book-description element,
// preserving its <br>-separated paragraph breaks as newlines and stripping any
// other inline markup. Returns "" when the element is absent.
func lcDescription(doc *goquery.Document) string {
	sel := doc.Find("#book-description").First()
	if sel.Length() == 0 {
		return ""
	}
	h, err := sel.Html()
	if err != nil {
		return strings.TrimSpace(sel.Text())
	}
	h = lcBrRE.ReplaceAllString(h, "\n")
	h = lcTagRE.ReplaceAllString(h, "")
	return strings.TrimSpace(html.UnescapeString(h))
}

// lcLD is the subset of a JSON-LD node parseLCBook reads. @type and image are
// raw because the site emits them as either a scalar or a list.
type lcLD struct {
	Type          json.RawMessage `json:"@type"`
	Description   string          `json:"description"`
	DatePublished string          `json:"datePublished"`
	Image         json.RawMessage `json:"image"`
	Graph         []lcLD          `json:"@graph"`
}

func (n lcLD) isBook() bool {
	return strings.Contains(strings.ToLower(string(n.Type)), "book")
}

// imageURL pulls a cover URL out of a JSON-LD image field, which may be a bare
// string, an ImageObject ({"url":…}), or a list of either.
func (n lcLD) imageURL() string {
	if len(n.Image) == 0 || string(n.Image) == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(n.Image, &str) == nil && str != "" {
		return str
	}
	var strs []string
	if json.Unmarshal(n.Image, &strs) == nil {
		for _, s := range strs {
			if s != "" {
				return s
			}
		}
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(n.Image, &obj) == nil && obj.URL != "" {
		return obj.URL
	}
	var objs []struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(n.Image, &objs) == nil {
		for _, o := range objs {
			if o.URL != "" {
				return o.URL
			}
		}
	}
	return ""
}

// parseLCJSONLD finds the Book node in one <script type="application/ld+json">
// block, which may hold a single object, an array of objects, or an object with
// an @graph list.
func parseLCJSONLD(raw string) (lcLD, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return lcLD{}, false
	}
	var arr []lcLD
	if json.Unmarshal([]byte(raw), &arr) == nil {
		for _, n := range arr {
			if n.isBook() {
				return n, true
			}
		}
		return lcLD{}, false
	}
	var one lcLD
	if json.Unmarshal([]byte(raw), &one) != nil {
		return lcLD{}, false
	}
	if one.isBook() {
		return one, true
	}
	for _, n := range one.Graph {
		if n.isBook() {
			return n, true
		}
	}
	return lcLD{}, false
}

// lcDetailDate reads the "Data wydania" (publication date) value from the
// detail-list markup — the DOM fallback for when JSON-LD carries no
// datePublished.
func lcDetailDate(doc *goquery.Document) string {
	date := ""
	doc.Find("dt").EachWithBreak(func(_ int, dt *goquery.Selection) bool {
		if strings.Contains(strings.ToLower(dt.Text()), "data wydania") {
			date = strings.TrimSpace(dt.Next().Text())
			return false
		}
		return true
	})
	return date
}

// --- author bibliography (Polish release detection, #43) ---

// AuthorSearch resolves an author name to their Lubimyczytać author-page path
// (/autor/<id>/<slug>), or "" if not found. Lubimyczytać's /szukaj/autor is
// JS-only noise, but /szukaj/autorzy server-renders real author tiles.
func (c *LCClient) AuthorSearch(name string) string {
	doc, err := c.fetch("/szukaj/autorzy?phrase=" + url.QueryEscape(name))
	if err != nil {
		c.warn(err)
		return ""
	}
	href, _ := doc.Find("a.newsBoxBook__title[href*='/autor/']").First().Attr("href")
	return lcPath(href, lcAuthorHrefRE)
}

// AuthorWorks fetches an author's Lubimyczytać bibliography (first page) and
// returns it as catalog Works — the Polish-release source that OL's fragmented
// language:null records can't provide. Only the first page is read (the most
// recent/popular titles), which is enough for release detection while staying
// polite; deep back-catalogue paging (/book/getMoreBooksToAuthorList) is left
// out on purpose.
func (c *LCClient) AuthorWorks(authorPath string) ([]Work, error) {
	doc, err := c.fetch(authorPath)
	if err != nil {
		return nil, err
	}
	return parseLCBibliography(doc), nil
}

// --- HTTP plumbing (mirrors GRClient) ---

func (c *LCClient) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.minGap - time.Since(c.lastReq); wait > 0 {
		time.Sleep(wait)
	}
	c.lastReq = time.Now()
}

func (c *LCClient) warn(err error) {
	c.warnOnce.Do(func() {
		log.Printf("lubimyczytac: enrichment unavailable (%v) — Polish clustering disabled", err)
	})
}

func (c *LCClient) fetch(path string) (*goquery.Document, error) {
	c.throttle()
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lubimyczytac: status %d fetching %s", resp.StatusCode, path)
	}
	return goquery.NewDocumentFromReader(io.LimitReader(resp.Body, lcMaxBody))
}

// --- pure parse helpers (fixture-tested) ---

var (
	lcAuthorHrefRE = regexp.MustCompile(`/autor/(\d+)/[^"'\s]*`)
	lcSeriesIDRE   = regexp.MustCompile(`/cykl/(\d+)/`)
	// lcTomRE captures the leading integer of a series-position label ("tom 1",
	// "tom 1.1", "tom 2.2"). A split multi-volume edition is labeled with a
	// sub-number (X.1, X.2, …) while the single-volume printing of the same
	// original work is just "X" — the leading integer is what actually matches
	// the book's real numeric position (verified against the /ksiazka page's
	// JSON-LD "position" field, which is "X" for both), so truncating recovers
	// the true position without fetching that page.
	lcTomRE = regexp.MustCompile(`tom\s*(\d+)`)
)

// lcSeriesKey reads a book-card's series/cycle detail — present on both
// search-result and author-bibliography tiles — into the dedup key every volume
// of that series position shares ("cykl:<seriesID>#<position>"), or "" if the
// card names no series. Shared by lcSearchHits and parseLCBibliography so a
// series id is never used alone as an identity: two different volumes of one
// series (e.g. book 1 and book 2 of a cycle) must never collide under the same
// key, which they would without the position suffix.
func lcSeriesKey(card *goquery.Selection) string {
	cycle := card.Find(".book-card__detail--cycle a").First()
	href, ok := cycle.Attr("href")
	if !ok {
		return ""
	}
	id := firstSubmatch(lcSeriesIDRE, href)
	if id == "" {
		return ""
	}
	pos := firstSubmatch(lcTomRE, cycle.Text())
	if pos == "" {
		return "cykl:" + id
	}
	return "cykl:" + id + "#" + pos
}

// lcSearchHit is one book-card search-result tile, as-parsed: work id (series
// position if the card names one, else the bare book id), title, cover and
// author — everything a match needs with no further fetch. BookID and Href are
// the bare book id and /ksiazka page path the add-a-book fallback needs as the
// note's Work ID + Link (issue #60); the clustering path ignores them and keys
// on WorkID (the series-position dedup key) instead.
type lcSearchHit struct {
	WorkID   string
	BookID   string
	Href     string
	Title    string
	CoverURL string
	Author   string
}

// lcSearchHits parses every `div.book-card` tile in search results into a hit,
// ranked by title-token overlap with `title` (best first). Scoping the selector
// to `.book-card` (not any `/ksiazka/` link) already excludes the promo/
// recommendation carousel a results page leads with — those use a different tile
// class (`result-tile`) — so the ranking only needs to disambiguate between
// multiple genuine hits (e.g. several editions or same-titled books).
func lcSearchHits(doc *goquery.Document, title string) []lcSearchHit {
	want := lcTokens(title)
	type scored struct {
		hit   lcSearchHit
		score int
	}
	var cands []scored
	doc.Find("div.book-card[data-book-id]").Each(func(_ int, card *goquery.Selection) {
		id, _ := card.Attr("data-book-id")
		id = strings.TrimSpace(id)
		titleLink := card.Find("a.book-card__title").First()
		t := strings.TrimSpace(titleLink.Text())
		if id == "" || t == "" {
			return
		}
		score := 0
		for tok := range lcTokens(t) {
			if want[tok] {
				score++
			}
		}
		if score == 0 {
			return
		}
		workID := lcSeriesKey(card)
		if workID == "" {
			workID = "lc:" + id
		}
		hit := lcSearchHit{WorkID: workID, BookID: id, Title: t}
		if href, ok := titleLink.Attr("href"); ok {
			hit.Href = strings.TrimSpace(href)
		}
		if src, ok := card.Find("img.book-card__cover-image").First().Attr("src"); ok {
			hit.CoverURL = strings.TrimSpace(src)
		}
		hit.Author = strings.TrimSpace(card.Find(".book-card__author").First().Text())
		cands = append(cands, scored{hit, score})
	})
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	out := make([]lcSearchHit, len(cands))
	for i, c := range cands {
		out[i] = c.hit
	}
	return out
}

// parseLCBibliography reads the book-card tiles on an author page into Works —
// title, cover, first-publish year and the series+position key used as the
// dedup id (lcSeriesKey — a bare series id would collide two different volumes
// of one series under the same identity).
func parseLCBibliography(doc *goquery.Document) []Work {
	var out []Work
	doc.Find("div.book-card").Each(func(_ int, card *goquery.Selection) {
		title := strings.TrimSpace(card.Find("a.book-card__title").First().Text())
		if title == "" {
			return
		}
		w := Work{Title: title, WorkID: lcSeriesKey(card)}
		if src, ok := card.Find("img.book-card__cover-image").First().Attr("src"); ok {
			w.CoverURL = strings.TrimSpace(src)
		}
		if y := card.Find(".book-card__detail--date .book-card__highlighted").First().Text(); y != "" {
			w.FirstPubYear = lcYear(y)
		}
		if w.WorkID == "" {
			if id, ok := card.Attr("data-book-id"); ok {
				w.WorkID = "lc:" + strings.TrimSpace(id)
			}
		}
		out = append(out, w)
	})
	return out
}

// lcFold maps Polish diacritics to their ASCII base and lowercases, so a title
// and a same-book alternate spelling tokenize to the same words.
func lcFold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'ą':
			b.WriteRune('a')
		case 'ć':
			b.WriteRune('c')
		case 'ę':
			b.WriteRune('e')
		case 'ł':
			b.WriteRune('l')
		case 'ń':
			b.WriteRune('n')
		case 'ó':
			b.WriteRune('o')
		case 'ś':
			b.WriteRune('s')
		case 'ź', 'ż':
			b.WriteRune('z')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lcTokens splits a folded string into the set of its alphanumeric tokens of
// length >= 2 (dropping single letters like the "i"/"ii" volume markers).
func lcTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.FieldsFunc(lcFold(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(t) >= 2 {
			out[t] = true
		}
	}
	return out
}

// lcPath extracts the relative resource path matched by re from an href that may
// be absolute (https://lubimyczytac.pl/…) or already relative.
func lcPath(href string, re *regexp.Regexp) string {
	return re.FindString(href)
}

// lcYear pulls a 4-digit year out of a date/year string ("2022", "2021-10-29").
func lcYear(s string) int {
	m := regexp.MustCompile(`\d{4}`).FindString(s)
	y, _ := strconv.Atoi(m)
	return y
}
