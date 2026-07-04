// Package scraper fetches a page and extracts fields driven by a Rules set
// (selectors/regex). Source-agnostic — the site-specific rules come from the
// DB (see package sources); DefaultRules carries the jnovels fallback.
package scraper

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// maxBodyBytes caps a fetched page so a runaway/huge response can't balloon
// memory. Novel pages are well under this.
const maxBodyBytes = 8 << 20 // 8 MiB

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
		if s := paragraphs(doc.Find(sel).First()); s != "" {
			return s
		}
	}
	// Fallback for older jnovels pages that have no synopsis container at all:
	// the synopsis is loose paragraphs sitting after a "SYNOPSIS" label (e.g.
	// `<p><b>SYNOPSIS</b></p>` then the blurb `<p>`s, then an "Associated Names"
	// heading or the volume <ol>). Not expressible as a stored CSS selector, so
	// it lives here as a last resort after the configured selectors miss.
	return synopsisAfterLabel(doc)
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
