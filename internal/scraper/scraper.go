// Package scraper fetches a page and extracts fields driven by a Rules set
// (selectors/regex). Source-agnostic — the site-specific rules come from the
// DB (see package sources); DefaultRules carries the jnovels fallback.
package scraper

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// maxBodyBytes caps a fetched page so a runaway/huge response can't balloon
// memory. Novel pages are well under this.
const maxBodyBytes = 8 << 20 // 8 MiB

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
	return &Client{http: &http.Client{Timeout: timeout}, userAgent: userAgent}
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
	return ""
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
