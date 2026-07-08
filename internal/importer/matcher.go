// Package importer resolves a Calibre library item to a tracked identity for the
// Calibre import (milestone 1.2.0). Given one calibre.Book it decides what kind
// of thing it is — a light-novel series, a light-novel volume, a regular English
// book, or a regular Polish book — and resolves it to a real catalog/scrape link
// with a confidence. Matching NEVER blocks the import: an uncertain result falls
// back to a synthetic https:// link, is flagged unmatched, and keeps the top 2–3
// candidate URLs so the reviewer can fix it by hand (#73).
//
// It is pure dispatch over injected providers — the OpenLibrary catalog (ISBN
// first, then title+author), the Lubimyczytać Polish catalog (title+author), and
// the jnovels light-novel search. No vault writes and no HTTP live here; the
// providers own that, and the package is exercised in tests against stubs. The
// staging model and note writer that consume a Result live in #74; the
// orchestration that groups LN volumes by series and drives concurrency in #75.
package importer

import (
	"sort"
	"strings"
	"sync"
	"time"

	"bookwatch/internal/calibre"
	"bookwatch/internal/provider"
	"bookwatch/internal/scraper"
)

// Kind is what a Calibre item was classified as, which picks the match backend.
type Kind int

const (
	KindUnknown  Kind = iota
	KindLNSeries      // a light novel matched as a whole series → jnovels
	KindLNVolume      // one owned volume of a series → archived, never matched (#74)
	KindEnglish       // a regular English book → OpenLibrary
	KindPolish        // a regular Polish book → Lubimyczytać
)

func (k Kind) String() string {
	switch k {
	case KindLNSeries:
		return "ln-series"
	case KindLNVolume:
		return "ln-volume"
	case KindEnglish:
		return "english"
	case KindPolish:
		return "polish"
	default:
		return "unknown"
	}
}

// MarshalText renders Kind as its slug so a Result serializes to readable JSON.
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// Confidence grades a match. Confident is an ISBN-exact hit or a top candidate
// that clears the author/title sanity check; Uncertain is everything else and
// always pairs with a synthetic link + fallback candidates.
type Confidence string

const (
	Confident Confidence = "confident"
	Uncertain Confidence = "uncertain"
)

// Candidate is one fallback the reviewer can promote when a match is uncertain:
// the source's own title + URL. Kept small — the note writer (#74) drops these
// into the staged note body.
type Candidate struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Result is the matcher's per-item output. On a confident match ResolvedLink is
// the real catalog/scrape URL and WorkID the catalog id (empty for jnovels,
// whose Link is its identity). On an uncertain match ResolvedLink is a synthetic
// https:// URL, Unmatched is true, and Candidates holds the top fallbacks. Err
// is set (and everything else left zero) only on a network/status failure, kept
// distinct from an uncertain match so the session (#75) can offer "retry
// errored" without re-running clean misses.
type Result struct {
	Kind         Kind        `json:"kind"`
	ResolvedLink string      `json:"resolved_link"`
	WorkID       string      `json:"work_id"`
	Confidence   Confidence  `json:"confidence"`
	Unmatched    bool        `json:"unmatched"`
	Candidates   []Candidate `json:"candidates"`
	Err          error       `json:"-"`
}

// OLSearcher is the OpenLibrary side the English backend needs: ISBN-first, then
// a fuzzy title search. *provider.OLClient satisfies it; tests inject a stub.
type OLSearcher interface {
	ByISBN(isbn string) (provider.Candidate, error)
	SearchByTitle(q string) ([]provider.Candidate, error)
}

// PolishSearcher is the Lubimyczytać side the Polish backend needs. It has no
// ISBN path and never errors (a network failure degrades to no candidates), so
// unlike OL it can't surface a retryable per-item error. *provider.LCClient
// satisfies it.
type PolishSearcher interface {
	SearchCandidates(title string) []provider.LCSearchResult
}

// LNSearcher is the jnovels title search the light-novel backend needs.
// *scraper.Client satisfies it via SearchTitle.
type LNSearcher interface {
	SearchTitle(query string) ([]scraper.SearchResult, error)
}

// Matcher dispatches a Calibre item to the right backend and applies the
// confidence gate. minGap is a global politeness throttle: each outbound backend
// call waits at least minGap since the previous one, so the ~146 regular lookups
// plus the jnovels scrapes can't hammer OL/jnovels into a block. It gates every
// call the same way, which also serializes concurrent callers — the small
// worker pool #75 layers on top stays polite without extra plumbing. Set minGap
// to 0 in tests. (Lubimyczytać already self-throttles, so its own ~700ms gap
// stacks on top; that's fine — this is a floor, not a budget.)
type Matcher struct {
	ol OLSearcher
	lc PolishSearcher
	ln LNSearcher

	minGap  time.Duration
	mu      sync.Mutex
	lastReq time.Time
}

// New builds a Matcher over the three backends. Any may be nil — a nil backend
// makes its kind fall straight to an unmatched result rather than panicking, so
// a partially-configured import still completes.
func New(ol OLSearcher, lc PolishSearcher, ln LNSearcher, minGap time.Duration) *Matcher {
	return &Matcher{ol: ol, lc: lc, ln: ln, minGap: minGap}
}

// Match classifies a Calibre item and resolves it. LN volumes (a light novel
// that belongs to a series) are returned unresolved with Kind KindLNVolume — the
// series they belong to is matched once by MatchSeries, and each volume becomes
// an untracked archive note (#74). A standalone light novel (no series) is
// matched by its own title as a series.
func (m *Matcher) Match(b calibre.Book) Result {
	switch classify(b) {
	case KindLNVolume:
		return Result{Kind: KindLNVolume} // archived, never catalog-matched
	case KindLNSeries:
		return m.matchLN(b.Title)
	case KindPolish:
		return m.matchPolish(b)
	default:
		return m.matchEnglish(b)
	}
}

// MatchSeries resolves a light-novel series by name to a jnovels link. #75 groups
// a series' Calibre volumes and calls this once for the whole series (Match
// routes a standalone LN book here too). author is accepted for symmetry but
// unused — jnovels search hits carry no author to check against.
func (m *Matcher) MatchSeries(name, _ string) Result { return m.matchLN(name) }

// Classify exposes the internal classification for the orchestration (#75),
// which groups items by kind before matching.
func Classify(b calibre.Book) Kind { return classify(b) }

// classify decides the backend for an item. A "Light Novel"-tagged book with a
// series is one owned volume (archived); without a series it's a standalone LN
// matched as its own series. A non-LN book routes to Polish when it looks Polish
// (a lubimyczytac id, or pol without eng), else to the English OpenLibrary path.
func classify(b calibre.Book) Kind {
	if hasTag(b.Tags, "light novel") {
		if strings.TrimSpace(b.Series) != "" {
			return KindLNVolume
		}
		return KindLNSeries
	}
	if looksPolish(b) {
		return KindPolish
	}
	return KindEnglish
}

// --- English: OpenLibrary, ISBN-first then title+author ---

func (m *Matcher) matchEnglish(b calibre.Book) Result {
	r := Result{Kind: KindEnglish}
	if m.ol == nil {
		return unmatch(r, b.Title, nil)
	}

	// 1. ISBN exact match — the highest-confidence path, no fuzzy check needed.
	if isbn := b.Identifiers["isbn"]; isbn != "" {
		m.throttle()
		cand, err := m.ol.ByISBN(isbn)
		if err != nil {
			r.Err = err
			return r
		}
		if cand.WorkID != "" {
			r.ResolvedLink = cand.OLURL
			r.WorkID = cand.WorkID
			r.Confidence = Confident
			return r
		}
	}

	// 2. Fuzzy title+author search.
	m.throttle()
	cands, err := m.ol.SearchByTitle(b.Title)
	if err != nil {
		r.Err = err
		return r
	}
	best, confident, fallbacks := pickOLBest(cands, b.Title, b.Authors)
	if confident {
		r.ResolvedLink = best.OLURL
		r.WorkID = best.WorkID
		r.Confidence = Confident
		return r
	}
	return unmatch(r, b.Title, fallbacks)
}

// --- Polish: Lubimyczytać, title+author ---

func (m *Matcher) matchPolish(b calibre.Book) Result {
	r := Result{Kind: KindPolish}
	if m.lc == nil {
		return unmatch(r, b.Title, nil)
	}
	m.throttle()
	hits := m.lc.SearchCandidates(b.Title) // never errors: a miss is no candidates
	best, confident, fallbacks := pickLCBest(hits, b.Title, b.Authors)
	if confident {
		r.ResolvedLink = best.URL
		r.WorkID = best.BookID
		r.Confidence = Confident
		return r
	}
	return unmatch(r, b.Title, fallbacks)
}

// --- Light novel: jnovels title search ---

func (m *Matcher) matchLN(query string) Result {
	r := Result{Kind: KindLNSeries}
	query = strings.TrimSpace(query)
	if m.ln == nil || query == "" {
		return unmatch(r, query, nil)
	}
	m.throttle()
	hits, err := m.ln.SearchTitle(query)
	if err != nil {
		r.Err = err
		return r
	}
	best, confident, fallbacks := pickLNBest(hits, query)
	if confident {
		r.ResolvedLink = best.URL // WorkID stays "" — the jnovels Link is the identity
		r.Confidence = Confident
		return r
	}
	return unmatch(r, query, fallbacks)
}

// unmatch fills r as an uncertain fallback: a synthetic (non-resolving) link, the
// unmatched flag, and whatever candidate URLs the search turned up.
func unmatch(r Result, title string, fallbacks []Candidate) Result {
	r.Confidence = Uncertain
	r.Unmatched = true
	r.ResolvedLink = syntheticLink(title)
	r.Candidates = fallbacks
	return r
}

// --- candidate ranking / confidence gate ---

// maxFallbacks caps how many candidate URLs an uncertain match keeps.
const maxFallbacks = 3

// pickOLBest ranks OpenLibrary title-search candidates and decides confidence.
// Candidates sort by author match, then exact normalized-title match, then title
// token overlap. The top is Confident only when it matches the author AND is a
// strong title match, AND no runner-up is comparably plausible — several equally
// good hits are treated as uncertain (issue #73's "several plausible" case).
func pickOLBest(cands []provider.Candidate, title string, authors []string) (provider.Candidate, bool, []Candidate) {
	want := titleTokens(title)
	ss := make([]olScored, 0, len(cands))
	for _, c := range cands {
		ss = append(ss, olScored{
			c:        c,
			authorOK: anyAuthorMatch(authors, c.Author),
			exact:    normTitle(c.Title) == normTitle(title),
			overlap:  tokenOverlap(want, titleTokens(c.Title)),
		})
	}
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		if a.authorOK != b.authorOK {
			return a.authorOK
		}
		if a.exact != b.exact {
			return a.exact
		}
		return a.overlap > b.overlap
	})

	fallbacks := make([]Candidate, 0, maxFallbacks)
	for i, s := range ss {
		if i >= maxFallbacks {
			break
		}
		fallbacks = append(fallbacks, Candidate{Title: s.c.Title, URL: s.c.OLURL})
	}
	if len(ss) == 0 {
		return provider.Candidate{}, false, nil
	}
	top := ss[0]
	if top.authorOK && (top.exact || top.overlap >= 2) {
		if len(ss) == 1 || !comparableOL(top, ss[1]) {
			return top.c, true, fallbacks
		}
	}
	return provider.Candidate{}, false, fallbacks
}

// olScored is a title-search candidate annotated with the signals the confidence
// gate ranks on.
type olScored struct {
	c        provider.Candidate
	authorOK bool
	exact    bool
	overlap  int
}

// comparableOL reports whether the runner-up is as plausible as the top — same
// author match, same-or-better exactness and overlap — which makes the pick
// ambiguous and the whole match uncertain.
func comparableOL(top, next olScored) bool {
	return next.authorOK && (next.exact || !top.exact) && next.overlap >= top.overlap
}

// pickLCBest ranks Lubimyczytać hits (already token-ranked best-first by the
// client) and applies the same author + title-overlap gate as the English path.
func pickLCBest(hits []provider.LCSearchResult, title string, authors []string) (provider.LCSearchResult, bool, []Candidate) {
	want := titleTokens(title)
	fallbacks := make([]Candidate, 0, maxFallbacks)
	for i, h := range hits {
		if i >= maxFallbacks {
			break
		}
		fallbacks = append(fallbacks, Candidate{Title: h.Title, URL: h.URL})
	}
	if len(hits) == 0 {
		return provider.LCSearchResult{}, false, nil
	}
	top := hits[0]
	topOverlap := tokenOverlap(want, titleTokens(top.Title))
	authorOK := anyAuthorMatch(authors, top.Author)
	if authorOK && topOverlap >= 2 {
		// Ambiguous only if a runner-up also matches the author with as much
		// title overlap.
		if len(hits) == 1 || !(anyAuthorMatch(authors, hits[1].Author) &&
			tokenOverlap(want, titleTokens(hits[1].Title)) >= topOverlap) {
			return top, true, fallbacks
		}
	}
	return provider.LCSearchResult{}, false, fallbacks
}

// pickLNBest applies the jnovels confidence gate. There's no author to check, and
// the scraper already ranks the aggregate series page ahead of individual volume
// pages, so the pick is simply hits[0]; the gate is how well it covers the query.
// The query is a full Calibre series name, so a top hit covering at least
// two-thirds of its tokens is a confident match — a "strictly ahead of runner-up"
// rule would misfire here, since every volume page of a series shares the
// series-name token and would tie the aggregate. A weak-coverage top (the series
// isn't really on jnovels, so search returns tangential posts) stays uncertain —
// safe, since that just stages a synthetic link + candidates for review.
func pickLNBest(hits []scraper.SearchResult, query string) (scraper.SearchResult, bool, []Candidate) {
	want := titleTokens(query)
	fallbacks := make([]Candidate, 0, maxFallbacks)
	for i, h := range hits {
		if i >= maxFallbacks {
			break
		}
		fallbacks = append(fallbacks, Candidate{Title: h.Title, URL: h.URL})
	}
	if len(hits) == 0 || len(want) == 0 {
		return scraper.SearchResult{}, false, fallbacks
	}
	top := hits[0]
	if tokenOverlap(want, titleTokens(top.Title))*3 >= len(want)*2 { // >= 2/3 covered
		return top, true, fallbacks
	}
	return scraper.SearchResult{}, false, fallbacks
}

// --- classification helpers ---

// hasTag reports whether tags contains want, case-insensitively.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// looksPolish routes a regular book to the Lubimyczytać backend: it carries a
// lubimyczytac identifier, or its languages have pol but not eng. A book owned in
// both languages stays English here (its eng edition is the OL-matchable one);
// the dual-language split is #74/#75's concern.
func looksPolish(b calibre.Book) bool {
	if strings.TrimSpace(b.Identifiers["lubimyczytac"]) != "" {
		return true
	}
	hasPol, hasEng := false, false
	for _, l := range b.Languages {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "pol":
			hasPol = true
		case "eng":
			hasEng = true
		}
	}
	return hasPol && !hasEng
}

// --- text helpers (self-contained; mirror the provider/scraper tokenizers) ---

var afterDashRE = replacerAfterDash()

// normTitle collapses a title to a comparison key: lowercased, subtitle/edition
// suffix after a dash or paren dropped, a leading article stripped, whitespace
// collapsed. Deliberately small — a local copy so the matcher doesn't depend on
// provider internals.
func normTitle(t string) string {
	s := strings.ToLower(strings.TrimSpace(t))
	if i := afterDashRE(s); i >= 0 {
		s = s[:i]
	}
	for _, a := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(s, a) {
			s = s[len(a):]
			break
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

// replacerAfterDash returns a function giving the index of the first " - ",
// " – ", " — ", or " (" separator (a subtitle/series/edition boundary), or -1.
func replacerAfterDash() func(string) int {
	seps := []string{" - ", " – ", " — ", " ("}
	return func(s string) int {
		best := -1
		for _, sep := range seps {
			if i := strings.Index(s, sep); i >= 0 && (best < 0 || i < best) {
				best = i
			}
		}
		return best
	}
}

// titleTokens is the set of a title's lowercase alphanumeric tokens of length
// >= 2 (dropping single-letter volume markers) — the unit title overlap scores.
func titleTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(t) >= 2 {
			out[t] = true
		}
	}
	return out
}

// tokenOverlap counts how many of have's tokens appear in want.
func tokenOverlap(want, have map[string]bool) int {
	n := 0
	for t := range have {
		if want[t] {
			n++
		}
	}
	return n
}

// anyAuthorMatch reports whether the candidate author shares a name token with
// any of the item's authors — the wrong-book guard for a fuzzy title hit.
func anyAuthorMatch(authors []string, cand string) bool {
	if strings.TrimSpace(cand) == "" || len(authors) == 0 {
		return false
	}
	ct := nameTokens(cand)
	if len(ct) == 0 {
		return false
	}
	for _, a := range authors {
		for t := range nameTokens(a) {
			if ct[t] {
				return true
			}
		}
	}
	return false
}

// nameTokens splits a name into its lowercase alphabetic runs of length >= 3, so
// initials and punctuation drop out (mirrors provider.nameTokens).
func nameTokens(name string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	}) {
		if len(f) >= 3 {
			out[f] = true
		}
	}
	return out
}

// syntheticLink builds the non-resolving placeholder link an unmatched item gets.
// The .invalid TLD (RFC 2606) guarantees it never resolves, so it's obviously a
// stand-in the reviewer must replace — never mistaken for a real source URL.
func syntheticLink(title string) string {
	slug := slugify(title)
	if slug == "" {
		slug = "unknown"
	}
	return "https://unmatched.bookwatch.invalid/" + slug
}

// slugify lowercases and reduces a title to a-z0-9 runs joined by hyphens.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// throttle blocks until at least minGap has passed since the previous outbound
// call, then stamps now. A zero minGap disables it (tests).
func (m *Matcher) throttle() {
	if m.minGap <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if wait := m.minGap - time.Since(m.lastReq); wait > 0 {
		time.Sleep(wait)
	}
	m.lastReq = time.Now()
}
