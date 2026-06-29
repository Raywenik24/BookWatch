package provider

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"bookwatch/internal/scraper"
)

const gbBaseURL = "https://www.googleapis.com/books/v1"

// GBClient is a Google Books API client for cover lookup and (future)
// new-release enrichment. An empty APIKey uses anonymous access, which is
// sufficient for low-volume personal use.
type GBClient struct {
	baseURL  string
	apiKey   string
	http     *http.Client
	warnOnce sync.Once // logs a non-200 from GB just once per run, not per tile
}

func NewGoogleBooks(apiKey string, timeout time.Duration) *GBClient {
	return &GBClient{
		baseURL: gbBaseURL,
		apiKey:  apiKey,
		http:    scraper.NewGuardedHTTPClient(timeout),
	}
}

type gbImageLinks struct {
	Thumbnail      string `json:"thumbnail"`
	SmallThumbnail string `json:"smallThumbnail"`
}

type gbVolumeInfo struct {
	ImageLinks gbImageLinks `json:"imageLinks"`
}

type gbVolume struct {
	VolumeInfo gbVolumeInfo `json:"volumeInfo"`
}

type gbSearchResp struct {
	Items []gbVolume `json:"items"`
}

// CoverURL returns a thumbnail URL from Google Books for the given title and
// author. Returns empty string on miss or error — never blocks the caller.
//
// It tries a precise phrase match first (intitle/inauthor), then falls back to
// a looser query that keeps only the author constraint. Both are quoted so the
// qualifier binds to the whole phrase rather than just the first word — without
// quotes, `intitle:Eye for an Eye` only requires the title to contain "Eye".
func (c *GBClient) CoverURL(title, author string) string {
	if cover := c.search(phrase("intitle", title) + phrase("inauthor", author)); cover != "" {
		return cover
	}
	// Looser pass: drop the strict title qualifier (some editions don't carry
	// the exact phrase in their title metadata) but keep the author so the
	// cover that comes back still belongs to the right writer.
	if author != "" {
		if cover := c.search(quote(title) + " " + phrase("inauthor", author)); cover != "" {
			return cover
		}
	}
	return ""
}

// search runs one Volumes query and returns the first result that actually
// carries an image, https-rewritten. maxResults > 1 matters: GB's top hit often
// lacks a thumbnail even when a later edition has one.
func (c *GBClient) search(q string) string {
	rawURL := fmt.Sprintf("%s/volumes?q=%s&maxResults=5&printType=books&fields=items/volumeInfo/imageLinks", c.baseURL, url.QueryEscape(strings.TrimSpace(q)))
	if c.apiKey != "" {
		rawURL += "&key=" + url.QueryEscape(c.apiKey)
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "BookWatch/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Surface the reason once. A 429 here almost always means keyless quota
		// is exhausted/disabled — set BOOKWATCH_GB_KEY to a free Google Books
		// API key. Logged once per run so the lazy cover queue can't spam it.
		c.warnOnce.Do(func() {
			hint := ""
			if resp.StatusCode == http.StatusTooManyRequests && c.apiKey == "" {
				hint = " (keyless quota — set BOOKWATCH_GB_KEY)"
			}
			log.Printf("googlebooks: cover lookup got status %d%s", resp.StatusCode, hint)
		})
		return ""
	}

	var result gbSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	for _, it := range result.Items {
		links := it.VolumeInfo.ImageLinks
		thumb := links.Thumbnail
		if thumb == "" {
			thumb = links.SmallThumbnail
		}
		if thumb != "" {
			// GB returns http:// URLs; force HTTPS so they aren't blocked by
			// mixed-content rules or a CSP img-src that only allows https://.
			return strings.Replace(thumb, "http://", "https://", 1)
		}
	}
	return ""
}

// phrase builds a `qualifier:"value"` term, or "" when value is empty.
func phrase(qualifier, value string) string {
	if value == "" {
		return ""
	}
	return " " + qualifier + ":" + quote(value)
}

// quote wraps value in double quotes after stripping any of its own, so the GB
// query parser treats it as a single phrase.
func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, "") + `"`
}
