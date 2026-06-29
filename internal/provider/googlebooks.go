package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bookwatch/internal/scraper"
)

const gbBaseURL = "https://www.googleapis.com/books/v1"

// GBClient is a Google Books API client for cover lookup and (future)
// new-release enrichment. An empty APIKey uses anonymous access, which is
// sufficient for low-volume personal use.
type GBClient struct {
	apiKey string
	http   *http.Client
}

func NewGoogleBooks(apiKey string, timeout time.Duration) *GBClient {
	return &GBClient{
		apiKey: apiKey,
		http:   scraper.NewGuardedHTTPClient(timeout),
	}
}

type gbImageLinks struct {
	Thumbnail string `json:"thumbnail"`
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
func (c *GBClient) CoverURL(title, author string) string {
	q := "intitle:" + url.QueryEscape(title)
	if author != "" {
		q += "+inauthor:" + url.QueryEscape(author)
	}
	rawURL := fmt.Sprintf("%s/volumes?q=%s&maxResults=1&printType=books&fields=items/volumeInfo/imageLinks/thumbnail", gbBaseURL, q)
	if c.apiKey != "" {
		rawURL += "&key=" + url.QueryEscape(c.apiKey)
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "BookWatch/1.0")

	resp, err := c.http.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	defer resp.Body.Close()

	var result gbSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Items) == 0 {
		return ""
	}
	// GB returns http:// URLs; force HTTPS so they aren't blocked by mixed-content
	// rules or a CSP img-src that only allows https://.
	return strings.Replace(result.Items[0].VolumeInfo.ImageLinks.Thumbnail, "http://", "https://", 1)
}
