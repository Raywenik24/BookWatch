package provider

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"bookwatch/internal/scraper"

	"github.com/PuerkitoBio/goquery"
)

const (
	grBaseURL = "https://www.goodreads.com"
	grMaxBody = 8 << 20 // 8 MiB — a book page is ~800 KiB
	grMinGap  = 700 * time.Millisecond
)

// GRClient resolves an OpenLibrary work to the Goodreads work cluster it belongs
// to, by ISBN, so the baseline picker can collapse translation/edition dupes and
// backfill missing covers (#40). Goodreads retired its API in 2020 and now AWS-WAF
// challenges /search outright (HTTP 202, empty body), so search is unusable — but
// /book/isbn/<isbn> 301-redirects to a normal 200 book page, which carries the
// shared work id, cover, title and author. That ISBN entry point is the whole
// mechanism. Every request is rate-limited, sent through the SSRF-guarded shared
// client, and memoized per ISBN for the process lifetime. A miss or outage
// degrades to the OpenLibrary path — this is a supplement, never a replacement.
//
// Limits learned from live data (see [[Sources & Rules]] / the #40 log entry):
//   - Goodreads does not merge every translation under one work (Polish editions
//     are often a separate work), so some foreign tiles legitimately won't cluster.
//   - OL's per-work ISBN lists are dirty (an ISBN can point at an unrelated book),
//     so each resolved book's author is checked against the tracked author before
//     its work id is trusted.
type GRClient struct {
	baseURL   string
	http      *http.Client
	userAgent string
	minGap    time.Duration

	mu       sync.Mutex
	lastReq  time.Time
	cache    map[string]GRMatch // key: ISBN
	warnOnce sync.Once
}

// GRMatch is the cluster identity an ISBN resolves to on Goodreads: the work id
// every edition of that book shares (the dedup key), plus the resolved book's
// title, cover and author.
type GRMatch struct {
	WorkID   string
	Title    string
	CoverURL string
	Author   string
	Found    bool
}

// NewGoodreads creates a Goodreads ISBN-resolution client. A blank userAgent
// falls back to a browser-like string (the book/ISBN endpoints serve any UA, but
// a sensible one is polite).
func NewGoodreads(userAgent string, timeout time.Duration) *GRClient {
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (BookWatch/1.0; +https://github.com/Raywenik24/BookWatch)"
	}
	return &GRClient{
		baseURL:   grBaseURL,
		http:      scraper.NewGuardedHTTPClient(timeout),
		userAgent: userAgent,
		minGap:    grMinGap,
		cache:     map[string]GRMatch{},
	}
}

// MatchWork resolves a work to its Goodreads cluster by trying its ISBNs in turn,
// returning the first that resolves to a book whose author matches `author` (the
// dirty-ISBN guard). Per-ISBN results are memoized, so re-loading an author's
// bibliography is free. Never errors — a failure is a !Found miss. Implements
// Matcher.
func (c *GRClient) MatchWork(title, author string, isbns []string) GRMatch {
	for _, isbn := range isbns {
		isbn = normalizeISBN(isbn)
		if isbn == "" {
			continue
		}
		m := c.matchByISBN(isbn)
		if !m.Found {
			continue
		}
		if author != "" && !sameAuthor(author, m.Author) {
			continue // OL's ISBN list pointed at a different book — distrust it
		}
		return m
	}
	return GRMatch{}
}

func (c *GRClient) matchByISBN(isbn string) GRMatch {
	c.mu.Lock()
	if m, ok := c.cache[isbn]; ok {
		c.mu.Unlock()
		return m
	}
	c.mu.Unlock()

	m := c.fetchByISBN(isbn)

	c.mu.Lock()
	c.cache[isbn] = m
	c.mu.Unlock()
	return m
}

func (c *GRClient) fetchByISBN(isbn string) GRMatch {
	// The guarded client follows the /book/isbn 301 to the book page itself.
	doc, err := c.fetch("/book/isbn/" + isbn)
	if err != nil {
		c.warn(err)
		return GRMatch{}
	}
	m := parseGRBook(doc)
	m.Found = m.WorkID != ""
	return m
}

// CoverByISBN returns a Goodreads cover for the first of isbns that resolves, or
// "" — the covers-only path for an OL work with no cover_i and no Google Books
// hit. Reuses the per-ISBN cache.
func (c *GRClient) CoverByISBN(isbns []string) string {
	for _, isbn := range isbns {
		if isbn = normalizeISBN(isbn); isbn == "" {
			continue
		}
		if m := c.matchByISBN(isbn); m.Found && m.CoverURL != "" {
			return m.CoverURL
		}
	}
	return ""
}

// --- HTTP plumbing ---

// throttle blocks until at least minGap has elapsed since the previous request,
// so a burst of clustering lookups can't hammer Goodreads.
func (c *GRClient) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.minGap - time.Since(c.lastReq); wait > 0 {
		time.Sleep(wait)
	}
	c.lastReq = time.Now()
}

func (c *GRClient) warn(err error) {
	c.warnOnce.Do(func() {
		log.Printf("goodreads: enrichment unavailable (%v) — falling back to OpenLibrary only", err)
	})
}

func (c *GRClient) fetch(path string) (*goquery.Document, error) {
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
		return nil, fmt.Errorf("goodreads: status %d fetching %s", resp.StatusCode, path)
	}
	return goquery.NewDocumentFromReader(io.LimitReader(resp.Body, grMaxBody))
}

// --- pure parse helpers (fixture-tested) ---

var (
	grWorkIDRE    = regexp.MustCompile(`/work/editions/(\d+)`)
	grCoverSize   = regexp.MustCompile(`\._S[XY]\d+_(\.(?:jpg|jpeg|png|gif))$`)
	grSeriesNum   = regexp.MustCompile(`#([\d.]+)`)
	grAuthorSlug  = regexp.MustCompile(`/author/show/\d+\.([A-Za-z0-9_]+)`)
	grLDPersonRE  = regexp.MustCompile(`"@type":"Person","name":"([^"]+)"`)
)

// parseGRBook reads a Goodreads book page into a GRMatch (work id, title, cover,
// author). The work id — the dedup key every edition shares — appears as a
// "/work/editions/<id>" reference inside the page's embedded JSON (not as an
// <a href>, so it's matched by regex over the raw HTML); the cover and title come
// from Open Graph meta; the author from JSON-LD (falling back to the
// /author/show/ slug) for the dirty-ISBN guard.
func parseGRBook(doc *goquery.Document) GRMatch {
	var m GRMatch
	if html, err := doc.Html(); err == nil {
		m.WorkID = firstSubmatch(grWorkIDRE, html)
	}
	m.Title = stripSeries(grMeta(doc, "og:title"))
	if cov := grFullCover(grMeta(doc, "og:image")); !grPlaceholderCover(cov) {
		m.CoverURL = cov
	}
	m.Author = grAuthor(doc)
	return m
}

// grAuthor extracts the book's author name for the guard. JSON-LD carries it
// cleanly ("@type":"Person"); the /author/show/ slug is the fallback (underscores
// for spaces), which is enough for a surname comparison.
func grAuthor(doc *goquery.Document) string {
	for _, s := range doc.Find("script[type='application/ld+json']").Nodes {
		if s.FirstChild == nil {
			continue
		}
		if m := grLDPersonRE.FindStringSubmatch(s.FirstChild.Data); m != nil {
			return m[1]
		}
	}
	if href, ok := doc.Find("a[href*='/author/show/']").First().Attr("href"); ok {
		if slug := firstSubmatch(grAuthorSlug, href); slug != "" {
			return strings.ReplaceAll(slug, "_", " ")
		}
	}
	return ""
}

// grMeta reads an <meta property="..."> content attribute (og:title, og:image).
func grMeta(doc *goquery.Document, property string) string {
	v, _ := doc.Find("meta[property='" + property + "']").First().Attr("content")
	return strings.TrimSpace(v)
}

// grFullCover strips Goodreads' "._SX98_"/"._SY475_" size constraint so the cover
// comes back at full resolution.
func grFullCover(u string) string {
	if u == "" {
		return ""
	}
	return grCoverSize.ReplaceAllString(u, "$1")
}

// grPlaceholderCover reports whether u is Goodreads' grey "no photo available"
// sentinel (or empty) rather than a real cover.
func grPlaceholderCover(u string) bool {
	return u == "" || strings.Contains(u, "nophoto")
}

var grSeriesSuffix = regexp.MustCompile(`\s*\([^()]*` + grSeriesNum.String() + `[^()]*\)\s*$`)

// stripSeries drops a trailing " (Series Name, #N)" qualifier Goodreads appends to
// titles, leaving the bare book title.
func stripSeries(t string) string {
	return strings.TrimSpace(grSeriesSuffix.ReplaceAllString(t, ""))
}

// normalizeISBN strips spaces/hyphens so "978-0-345-51870-5" matches the bare form
// the /book/isbn/ path expects.
func normalizeISBN(isbn string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.TrimSpace(isbn))
}

// sameAuthor reports whether two author strings plausibly name the same person:
// they share a 3+-letter name token (case-insensitive). Deliberately loose — it
// only needs to reject an ISBN that resolved to a wholly different book, while
// tolerating reordering, initials, accents, and slug forms like "Brett-P". Both
// names empty-of-tokens is not a match.
func sameAuthor(a, b string) bool {
	tb := nameTokens(b)
	if len(tb) == 0 {
		return false
	}
	for t := range nameTokens(a) {
		if tb[t] {
			return true
		}
	}
	return false
}

// nameTokens splits a name into the set of its lowercase alphabetic runs of
// length >= 3 (so initials and punctuation drop out).
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

func firstSubmatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
