package server

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"bookwatch/internal/scraper"
	"bookwatch/internal/sources"
)

// Discover (#91) is a jnovels browse tab for finding new light novels — distinct
// from the library-only Randomizer. Two feeds: Latest (newest epub releases) and
// Find-new (jnovels' own /randomizer/, minus PDFs and books already tracked).
// Both are cached server-side with a TTL so tab switches don't re-scrape; the
// Refresh/Reroll buttons force a fresh fetch. Opening a pick resolves the
// spoiler-safe series page (volume-1 cover + description) on demand.

// discoverTTL is how long a cached feed is served before a background staleness
// forces a refetch. Latest moves slowly (a handful of releases a day) and the
// randomizer is deliberately re-rollable, so a generous window keeps jnovels
// from being hammered on every tab visit.
const discoverTTL = 30 * time.Minute

// discoverLatestCount is how many epub releases the Latest feed gathers.
const discoverLatestCount = 20

// discoverCoverHostRE limits the cover proxy to jnovels and its Jetpack/Photon
// CDN (i0.wp.com, i1.wp.com, …), so the proxy can't be turned into a general
// SSRF fetcher for arbitrary hosts.
var discoverCoverHostRE = regexp.MustCompile(`(?i)(^|\.)(jnovels\.com|wp\.com)$`)

// maxDiscoverCoverBytes caps a proxied cover; art is well under this.
const maxDiscoverCoverBytes = 8 << 20 // 8 MiB

// handleDiscoverLatest serves the newest jnovels epub releases (cached, TTL),
// forcing a fresh scrape on ?refresh=1.
func (s *Server) handleDiscoverLatest(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	rels, err := s.latestReleases(refresh)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"releases": rels}, nil)
}

// latestReleases returns the cached Latest feed, refetching when the cache is
// empty, stale, or refresh is set.
func (s *Server) latestReleases(refresh bool) ([]scraper.Release, error) {
	s.discoverMu.Lock()
	fresh := time.Since(s.discoverLatestAt) < discoverTTL && len(s.discoverLatest) > 0
	cached := s.discoverLatest
	s.discoverMu.Unlock()
	if fresh && !refresh {
		return cached, nil
	}
	rels, err := s.sc.LatestEpubReleases(discoverLatestCount)
	if err != nil {
		return nil, err
	}
	if rels == nil {
		rels = []scraper.Release{}
	}
	s.discoverMu.Lock()
	s.discoverLatest = rels
	s.discoverLatestAt = time.Now()
	s.discoverMu.Unlock()
	return rels, nil
}

// handleDiscoverFindNew serves the jnovels randomizer picks, minus PDFs and
// novels already in the library. ?refresh=1 rerolls.
func (s *Server) handleDiscoverFindNew(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	picks, err := s.findNewPicks(refresh)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"releases": picks}, nil)
}

// findNewPicks returns the cached Find-new feed, rerolling when empty, stale, or
// refresh is set. Each reroll hits /randomizer/ once and drops PDF picks and any
// pick whose series is already tracked.
func (s *Server) findNewPicks(refresh bool) ([]scraper.Release, error) {
	s.discoverMu.Lock()
	fresh := time.Since(s.discoverPicksAt) < discoverTTL && len(s.discoverPicks) > 0
	cached := s.discoverPicks
	s.discoverMu.Unlock()
	if fresh && !refresh {
		return cached, nil
	}
	raw, err := s.sc.RandomizerPicks()
	if err != nil {
		return nil, err
	}
	picks := filterFindNew(raw, s.trackedSeriesKeys())
	s.discoverMu.Lock()
	s.discoverPicks = picks
	s.discoverPicksAt = time.Now()
	s.discoverMu.Unlock()
	return picks, nil
}

// filterFindNew reduces raw /randomizer/ picks to the Find-new feed: jnovels
// mixes light-novel epub, pdf, and manga (mangacbz) posts in one randomizer, so
// this keeps only epub light-novel posts (their URLs end in "-epub") — which
// drops both the pdf twins and the manga in one pass — and skips any pick whose
// series is already tracked.
func filterFindNew(raw []scraper.Release, owned map[string]bool) []scraper.Release {
	picks := make([]scraper.Release, 0, len(raw))
	for _, r := range raw {
		if !strings.HasSuffix(strings.TrimRight(r.URL, "/"), "-epub") {
			continue // epub light-novel posts only (drops pdf + manga cbz)
		}
		if owned[seriesKey(r.URL)] {
			continue // already tracked
		}
		picks = append(picks, r)
	}
	return picks
}

// trackedSeriesKeys is the set of series keys for every light-novel book already
// tracked, so Find-new can skip picks the reader already has. Best effort: a
// store error just yields an empty set (nothing skipped).
func (s *Server) trackedSeriesKeys() map[string]bool {
	out := map[string]bool{}
	books, err := s.st.ListBooks()
	if err != nil {
		return out
	}
	for _, b := range books {
		if b.Kind == "book" || b.Link == "" {
			continue
		}
		if k := seriesKey(b.Link); k != "" {
			out[k] = true
		}
	}
	return out
}

// seriesVolumeRE strips the trailing volume/format markers off a jnovels slug so
// a volume post and its series page collapse to the same key: "…-volume-10-epub"
// and "…-light-novel-epub" both reduce to the bare series slug.
var seriesVolumeRE = regexp.MustCompile(`(?i)-(volume-\d+|light-novel)(-epub|-pdf)?$|-(epub|pdf)$`)

// seriesKey reduces a jnovels post URL to a stable series identifier — its last
// path segment with the volume-number and epub/pdf/light-novel suffixes removed
// — so a randomizer volume pick can be matched against a tracked series page.
func seriesKey(postURL string) string {
	slug := strings.Trim(postURL, "/")
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}
	// Peel markers repeatedly: "…-volume-10-epub" → "…-volume-10" → "…".
	for {
		next := seriesVolumeRE.ReplaceAllString(slug, "")
		if next == slug {
			break
		}
		slug = next
	}
	return strings.ToLower(slug)
}

// handleDiscoverResolve resolves a picked jnovels volume URL to its spoiler-safe
// series page: it follows the volume page's "Refer to original post" link to the
// series aggregate page and scrapes that page's title, cover, and description
// (the volume-1 view). The series URL is returned so the Add button can create
// the note through the normal light-novel add path.
func (s *Server) handleDiscoverResolve(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	if !discoverIsJnovelsURL(raw) {
		writeJSON(w, http.StatusBadRequest, errBody("not a jnovels url"))
		return
	}
	seriesURL, err := s.sc.OriginalPost(raw)
	if err != nil {
		// No original-post link (some posts are already the series page): fall
		// back to resolving the picked URL itself so the modal still shows data.
		seriesURL = raw
	}
	rl := sources.NewResolver(s.st).For(seriesURL)
	nd, err := s.sc.NovelData(seriesURL, rl)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	respond(w, map[string]any{
		"series_url":  seriesURL,
		"title":       nd.Title,
		"volumes":     nd.Volumes,
		"cover_url":   nd.CoverURL,
		"cover_data":  coverDataURI(nd.CoverURL),
		"description": nd.Description,
	}, nil)
}

// handleDiscoverCover proxies a jnovels/Photon cover image through the SSRF-guarded
// client so the grid can show covers without the page CSP whitelisting jnovels'
// image hosts. The URL is restricted to jnovels + its wp.com CDN.
func (s *Server) handleDiscoverCover(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	if raw == "" || !discoverIsCoverURL(raw) {
		http.NotFound(w, r)
		return
	}
	client := scraper.NewGuardedHTTPClient(15 * time.Second)
	resp, err := client.Get(raw)
	if err != nil {
		http.Error(w, "cover fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.NotFound(w, r)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.Copy(w, io.LimitReader(resp.Body, maxDiscoverCoverBytes))
}

// discoverIsJnovelsURL reports whether raw is an https jnovels.com post URL.
func discoverIsJnovelsURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "jnovels.com" || strings.HasSuffix(host, ".jnovels.com")
}

// discoverIsCoverURL reports whether raw is an https cover on jnovels or its
// wp.com Photon CDN.
func discoverIsCoverURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	return discoverCoverHostRE.MatchString(strings.ToLower(u.Hostname()))
}
