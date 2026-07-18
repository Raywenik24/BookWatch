// Package scraper fetches a page and extracts fields driven by a Rules set
// (selectors/regex). Source-agnostic — the site-specific rules come from the
// DB (see package sources); DefaultRules carries the jnovels fallback.
package scraper

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// maxBodyBytes caps a fetched page so a runaway/huge response can't balloon
// memory. Novel pages are well under this.
const maxBodyBytes = 8 << 20 // 8 MiB

// jnovelsBaseURL is the LN scrape target. Its WordPress search endpoint is
// ?s=<query>, which server-renders result posts (no API).
const jnovelsBaseURL = "https://jnovels.com"

// maxSearchResults caps how many ranked candidates a title search returns. The
// import matcher (#73) only keeps the top 2–3 as fallbacks, so a handful is
// plenty and bounds the noise the search hands downstream.
const maxSearchResults = 10

// maxRedirects caps how many redirects a fetch follows; every hop is re-checked
// by the SSRF guard (guardDial runs on each dial).
const maxRedirects = 5

// AllowPrivateHosts disables the SSRF guard that blocks fetches to
// loopback/private/link-local addresses. Off in production; the test suite sets
// it true (httptest binds to loopback), and BOOKWATCH_ALLOW_PRIVATE_FETCH=1
// enables it for a deliberately LAN-only deployment.
var AllowPrivateHosts = os.Getenv("BOOKWATCH_ALLOW_PRIVATE_FETCH") == "1"

// VolumeRE matches "VOLUME 12", "Volume12", etc. (default item regex).
var VolumeRE = regexp.MustCompile(`(?i)VOLUME\s*(\d+)`)

// Rules describe how to extract fields from a page.
type Rules struct {
	VolumeListSel string         // container holding the volume list; last match used
	VolumeItemSel string         // items within the list
	VolumeItemRE  *regexp.Regexp // pulls the volume number from an item
	TitleSel      string
	CoverSel      string
	CoverAttr     string   // attribute holding the cover URL (e.g. "src")
	DescSels      []string // ordered; first selector that yields text wins
}

// DefaultRules is the jnovels fallback (ported from the old Python).
func DefaultRules() Rules {
	return Rules{
		VolumeListSel: "ol",
		VolumeItemSel: "li",
		VolumeItemRE:  VolumeRE,
		TitleSel:      "h1.post-title.entry-title",
		CoverSel:      "div.featured-media img",
		CoverAttr:     "src",
		DescSels:      []string{"div.synopsis-description", "#editdescription"},
	}
}

type Client struct {
	http      *http.Client
	userAgent string
}

func New(userAgent string, timeout time.Duration) *Client {
	return &Client{http: NewGuardedHTTPClient(timeout), userAgent: userAgent}
}

// NewGuardedHTTPClient returns a client whose transport refuses to connect to
// loopback/private/link-local addresses (the SSRF guard, enforced at dial time
// so it also covers redirect targets and DNS rebinding) and that caps redirects.
// Shared by page fetches and cover downloads so both paths are guarded.
func NewGuardedHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guardDial}
	tr := http.DefaultTransport.(*http.Transport).Clone() // keep proxy/HTTP2/idle defaults
	tr.DialContext = dialer.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// guardDial runs after DNS resolution with the concrete IP:port about to be
// dialed, so it blocks SSRF targets even across redirects.
func guardDial(_, address string, _ syscall.RawConn) error {
	if AllowPrivateHosts {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("ssrf guard: cannot parse dial address %q", address)
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("ssrf guard: refusing to connect to %s", ip)
	}
	return nil
}

// isBlockedIP reports whether ip is one the server must never be tricked into
// fetching: loopback, private (RFC1918 + unique-local fc00::/7), link-local, or
// the unspecified address.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

func (c *Client) fetch(url string) (*goquery.Document, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxBodyBytes))
}

// LatestVolume returns the max volume number + item count using rl.
func (c *Client) LatestVolume(url string, rl Rules) (max int, count int, err error) {
	doc, err := c.fetch(url)
	if err != nil {
		return 0, 0, err
	}
	max, count = volumesFromDoc(doc, rl)
	return max, count, nil
}

// NovelData is everything needed to create a new note.
type NovelData struct {
	Title       string `json:"title"`
	CoverURL    string `json:"cover_url"`
	Description string `json:"description"`
	Volumes     int    `json:"volumes"`
}

func (c *Client) NovelData(url string, rl Rules) (NovelData, error) {
	doc, err := c.fetch(url)
	if err != nil {
		return NovelData{}, err
	}
	return parseNovel(doc, rl)
}

// NovelDataResolved scrapes url like NovelData, but first follows a jnovels
// single-volume post's "Refer to original post" link to the aggregate series
// page — the one carrying the full volume list — so the add flow creates a
// series note, not a lone-volume note (#89). Returns the data and the URL
// actually scraped (the aggregate when redirected, else url) so the caller can
// key the note on it.
func (c *Client) NovelDataResolved(url string, rl Rules) (NovelData, string, error) {
	doc, err := c.fetch(url)
	if err != nil {
		return NovelData{}, url, err
	}
	if orig := parseOriginalPost(doc); orig != "" && orig != url {
		doc2, err := c.fetch(orig)
		if err != nil {
			return NovelData{}, url, err
		}
		nd, err := parseNovel(doc2, rl)
		return nd, orig, err
	}
	nd, err := parseNovel(doc, rl)
	return nd, url, nil
}

// ParseNovelHTML parses from raw HTML (used by tests + live-test).
func ParseNovelHTML(html string, rl Rules) (NovelData, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return NovelData{}, err
	}
	return parseNovel(doc, rl)
}

func parseNovel(doc *goquery.Document, rl Rules) (NovelData, error) {
	title := strings.TrimSpace(doc.Find(rl.TitleSel).First().Text())
	if title == "" {
		return NovelData{}, fmt.Errorf("page structure not recognized (title)")
	}
	attr := rl.CoverAttr
	if attr == "" {
		attr = "src"
	}
	cover, _ := doc.Find(rl.CoverSel).First().Attr(attr)
	if cover == "" {
		return NovelData{}, fmt.Errorf("cover image not found")
	}
	vol, _ := volumesFromDoc(doc, rl)
	return NovelData{
		Title:       title,
		CoverURL:    cover,
		Description: extractDescription(doc, rl),
		Volumes:     vol,
	}, nil
}

func extractDescription(doc *goquery.Document, rl Rules) string {
	for _, sel := range rl.DescSels {
		if sel == "" {
			continue
		}
		if s := describeSelection(doc.Find(sel)); s != "" {
			return s
		}
	}
	// Fallback for older jnovels pages that have no synopsis container at all:
	// the synopsis is loose paragraphs sitting after a "SYNOPSIS" label (e.g.
	// `<p><b>SYNOPSIS</b></p>` then the blurb `<p>`s, then an "Associated Names"
	// heading or the volume <ol>). Not expressible as a stored CSS selector, so
	// it lives here as a last resort after the configured selectors miss.
	if s := synopsisAfterLabel(doc); s != "" {
		return s
	}
	// Fallback for jnovels pages where the label sits inline as a bold/span
	// prefix inside the same <p> as the blurb, e.g.
	// `<p><span><b>Description</b></span><br/>Zagan is feared...</p>`.
	return descriptionFromLabelPrefixParagraph(doc)
}

// synopsisAfterLabel finds a heading/paragraph whose text is just "SYNOPSIS"
// (or "DESCRIPTION") and returns the following sibling <p> paragraphs, stopping
// at the first non-<p> block (a heading or a list — where the volume table or
// "Associated Names" section begins).
func synopsisAfterLabel(doc *goquery.Document) string {
	var out []string
	doc.Find("h1, h2, h3, h4, h5, h6, p").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		label := strings.ToUpper(strings.Trim(strings.TrimSpace(s.Text()), ": "))
		if label != "SYNOPSIS" && label != "DESCRIPTION" {
			return true // keep looking
		}
		for n := s.Next(); n.Length() > 0; n = n.Next() {
			if goquery.NodeName(n) != "p" {
				break
			}
			n.Find("span").Remove()
			if t := strings.TrimSpace(n.Text()); t != "" {
				out = append(out, t)
			}
		}
		return false // found the label; stop scanning
	})
	return strings.Join(out, " ")
}

// descriptionFromLabelPrefixParagraph finds a <p> whose leading child is a
// <b>/<span> reading "Description" or "Synopsis" and returns the rest of that
// paragraph's text (the blurb sits as a loose text node after a <br/>, in the
// same <p> as the label — no following-sibling <p>s to walk).
func descriptionFromLabelPrefixParagraph(doc *goquery.Document) string {
	var result string
	doc.Find("p").EachWithBreak(func(_ int, p *goquery.Selection) bool {
		label := p.Find("b, span").First()
		if label.Length() == 0 {
			return true
		}
		text := strings.ToUpper(strings.TrimSpace(label.Text()))
		if text != "DESCRIPTION" && text != "SYNOPSIS" {
			return true
		}
		clone := p.Clone()
		clone.Find("b, span").First().Remove()
		if remaining := strings.TrimSpace(clone.Text()); remaining != "" {
			result = remaining
			return false
		}
		return true
	})
	return result
}

// describeSelection returns the best blurb text from a description-container
// selection. It prefers <p> paragraphs (the classic jnovels synopsis layout), but
// falls back to the container's own text when there are none — jnovels' newer
// Kobo-widget synopsis puts the blurb as a bare text node inside a nested
// `div.synopsis-description`, with no <p> to find. The longest candidate across
// all matched containers wins, so a wrapper holding only boilerplate loses to the
// one holding the actual synopsis.
func describeSelection(sel *goquery.Selection) string {
	best := ""
	sel.Each(func(_ int, s *goquery.Selection) {
		cand := paragraphs(s)
		if cand == "" {
			cand = directText(s)
		}
		if len(cand) > len(best) {
			best = cand
		}
	})
	return best
}

// directText returns a container's whitespace-collapsed text with obvious
// non-synopsis children removed (scripts, styles, links like "Continue Reading",
// and buttons) — the fallback when a synopsis container carries no <p> paragraphs.
func directText(sel *goquery.Selection) string {
	c := sel.Clone()
	c.Find("script, style, a, button, noscript").Remove()
	return strings.Join(strings.Fields(c.Text()), " ")
}

func paragraphs(container *goquery.Selection) string {
	var parts []string
	container.Find("p").Each(func(_ int, p *goquery.Selection) {
		p.Find("span").Remove()
		if t := strings.TrimSpace(p.Text()); t != "" {
			parts = append(parts, t)
		}
	})
	return strings.Join(parts, " ")
}

// SearchResult is one candidate jnovels post for a title search: the post title
// and its canonical URL. The list is fuzzy by nature — SearchTitle returns these
// ranked (best first) and the import matcher (#73) applies the confidence gate.
type SearchResult struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// SearchTitle resolves a light-novel title to candidate jnovels post URLs, best
// match first. jnovels has no API, so this scrapes its WordPress search page
// (one request per call) through the same guarded client the page fetches use.
// A search that finds nothing returns (nil, nil); a network/status failure
// returns the error.
func (c *Client) SearchTitle(query string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	doc, err := c.fetch(jnovelsBaseURL + "/?s=" + url.QueryEscape(query))
	if err != nil {
		return nil, err
	}
	return rankSearchResults(doc, query), nil
}

// ParseSearchHTML ranks candidates from raw jnovels search HTML (used by tests
// and the live-test tool, mirroring ParseNovelHTML).
func ParseSearchHTML(html, query string) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	return rankSearchResults(doc, query), nil
}

// singleVolumeRE detects a single-volume post title ("… Volume 3 Epub"). jnovels
// lists both an aggregate series page ("… Light Novel Epub", carrying the whole
// volume list — the page the add flow scrapes) and a post per individual volume;
// the aggregate is what the matcher wants, so a single-volume hit ranks below it.
var singleVolumeRE = regexp.MustCompile(`(?i)\bvolume\s*\d+`)

var (
	// downloadPrefixRE strips jnovels' "Download " post-title lead-in.
	downloadPrefixRE = regexp.MustCompile(`(?i)^\s*download\s+`)
	// formatSuffixRE strips a trailing format/type word ("Light Novel", "Epub",
	// "Pdf", …), applied repeatedly so "… Light Novel Epub" reduces fully.
	formatSuffixRE = regexp.MustCompile(`(?i)\s+(light novel|web novel|webnovel|epub|pdf)\s*$`)
)

// cleanSearchTitle makes a jnovels post title readable for the add-a-book picker:
// jnovels names its posts "Download <Title> Light Novel Epub", so strip the
// "Download" lead-in and the trailing format/type words. Display-only — the
// created note's title still comes from scraping the page (#89).
func cleanSearchTitle(title string) string {
	t := downloadPrefixRE.ReplaceAllString(title, "")
	for {
		n := formatSuffixRE.ReplaceAllString(t, "")
		if n == t {
			break
		}
		t = n
	}
	return strings.TrimSpace(t)
}

// volumeSuffixRE strips a trailing "Volume N" from a cleaned title, reducing a
// per-volume post title to its series title.
var volumeSuffixRE = regexp.MustCompile(`(?i)\s+volume\s*\d+\s*$`)

// seriesTitle reduces a cleaned post title to the series name by dropping a
// trailing "Volume N" ("Kumo Desu ga Nani ka Volume 7" → "Kumo Desu ga Nani ka").
func seriesTitle(clean string) string {
	return strings.TrimSpace(volumeSuffixRE.ReplaceAllString(clean, ""))
}

// CollapseToSeries dedups ranked search results down to one entry per series.
// jnovels lists a post per volume, all resolving to the same aggregate light-
// novel page, so the add-a-book picker shows the series once instead of every
// volume (#89). The best-ranked post per series is kept as the representative
// (the add flow resolves it to the aggregate via "Refer to original post"), with
// its Title rewritten to the series name. Input order is preserved. Kept out of
// rankSearchResults so the #73 import matcher still sees per-volume hits.
func CollapseToSeries(results []SearchResult) []SearchResult {
	seen := map[string]bool{}
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		st := seriesTitle(r.Title)
		key := strings.ToLower(st)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, SearchResult{Title: st, URL: r.URL})
	}
	return out
}

// rankSearchResults reads every search-result post link (h1.post-title.entry-title
// is a bare text node on a novel page but wraps an <a> in search results, so the
// child-anchor requirement selects only real hits, skipping the "Search Results
// for:" page heading). Webnovel-translation and pdf posts are dropped (BookWatch
// wants the official epub only). Remaining hits are ranked by query-token overlap,
// then aggregate before single-volume, then original page order — and deduped by
// URL. Titles are cleaned for display (see cleanSearchTitle).
func rankSearchResults(doc *goquery.Document, query string) []SearchResult {
	want := searchTokens(query)
	type scored struct {
		res        SearchResult
		overlap    int
		singleVol  bool
		formatRank int // 0 epub, 1 pdf, 2 other
		order      int
	}
	var cands []scored
	doc.Find("h1.post-title.entry-title > a[href]").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		href = strings.TrimSpace(href)
		title := strings.TrimSpace(a.Text())
		if href == "" || title == "" {
			return
		}
		// jnovels posts a webnovel-translation variant and a pdf twin alongside
		// each official light-novel epub; BookWatch only ever wants the epub, so
		// drop both — a webnovel post isn't the LN, and the add flow scrapes the
		// epub page (pdf-only misses are excluded too, per the jnovels design).
		low := strings.ToLower(title)
		if strings.Contains(low, "webnovel") || strings.Contains(low, "web novel") || formatRank(title) == 1 {
			return
		}
		overlap := 0
		for tok := range searchTokens(title) {
			if want[tok] {
				overlap++
			}
		}
		if overlap == 0 {
			return
		}
		// Rank on the raw title (its "volume N"/format words drive the tiebreaks),
		// but store the cleaned title for display.
		cands = append(cands, scored{
			res:        SearchResult{Title: cleanSearchTitle(title), URL: href},
			overlap:    overlap,
			singleVol:  singleVolumeRE.MatchString(title),
			formatRank: formatRank(title),
			order:      len(cands),
		})
	})
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.overlap != b.overlap {
			return a.overlap > b.overlap
		}
		if a.singleVol != b.singleVol {
			return !a.singleVol // aggregate series page before a single volume
		}
		if a.formatRank != b.formatRank {
			return a.formatRank < b.formatRank
		}
		return a.order < b.order
	})
	out := make([]SearchResult, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c.res.URL] {
			continue
		}
		seen[c.res.URL] = true
		out = append(out, c.res)
		if len(out) >= maxSearchResults {
			break
		}
	}
	return out
}

// formatRank orders a post title by download format so the epub page (the one
// the add flow scrapes) sorts ahead of its pdf twin.
func formatRank(title string) int {
	t := strings.ToLower(title)
	switch {
	case strings.Contains(t, "epub"):
		return 0
	case strings.Contains(t, "pdf"):
		return 1
	default:
		return 2
	}
}

// searchTokens splits a title/query into its set of lowercase alphanumeric tokens
// of length >= 2 (dropping single-letter volume markers), for overlap scoring.
func searchTokens(s string) map[string]bool {
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

func volumesFromDoc(doc *goquery.Document, rl Rules) (max, count int) {
	lists := doc.Find(rl.VolumeListSel)
	if lists.Length() == 0 {
		return 0, 0
	}
	last := lists.Eq(lists.Length() - 1)
	re := rl.VolumeItemRE
	if re == nil {
		re = VolumeRE
	}
	last.Find(rl.VolumeItemSel).Each(func(_ int, li *goquery.Selection) {
		count++
		if m := re.FindStringSubmatch(strings.TrimSpace(li.Text())); m != nil {
			if n, e := strconv.Atoi(m[1]); e == nil && n > max {
				max = n
			}
		}
	})
	return max, count
}
