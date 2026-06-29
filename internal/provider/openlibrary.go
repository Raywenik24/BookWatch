package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bookwatch/internal/scraper"
)

const (
	defaultBaseURL   = "https://openlibrary.org"
	defaultCoversURL = "https://covers.openlibrary.org"
	searchLimit      = 20
	worksLimit       = 500
	editionsLimit    = 100
)

// OLClient is an OpenLibrary catalog client. Implements Provider.
type OLClient struct {
	baseURL    string
	coversURL  string
	http       *http.Client
	userAgent  string
	retryDelay time.Duration // initial backoff delay; doubles on each 429
}

// NewOpenLibrary creates an OpenLibrary provider.
func NewOpenLibrary(userAgent string, timeout time.Duration) *OLClient {
	return &OLClient{
		baseURL:    defaultBaseURL,
		coversURL:  defaultCoversURL,
		http:       scraper.NewGuardedHTTPClient(timeout),
		userAgent:  userAgent,
		retryDelay: 500 * time.Millisecond,
	}
}

// getOnce makes one HTTP GET and decodes JSON into v.
// Returns (true, err) when the server responds 429 and the caller should retry.
func (c *OLClient) getOnce(rawURL string, v any) (retry bool, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return true, fmt.Errorf("rate limited")
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("openlibrary: status %d fetching %s", resp.StatusCode, rawURL)
	}
	return false, json.NewDecoder(resp.Body).Decode(v)
}

// get fetches rawURL with up to 3 attempts, doubling the delay on each 429.
func (c *OLClient) get(rawURL string, v any) error {
	delay := c.retryDelay
	for attempt := 0; attempt < 3; attempt++ {
		retry, err := c.getOnce(rawURL, v)
		if !retry {
			return err
		}
		if attempt < 2 && delay > 0 {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return fmt.Errorf("openlibrary: rate-limited after retries: %s", rawURL)
}

func (c *OLClient) url(path string) string { return c.baseURL + path }

func coverURL(coversBase string, id int) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprintf("%s/b/id/%d-L.jpg", coversBase, id)
}

// parseWorkID strips the "/works/" prefix from an OL key.
func parseWorkID(key string) string { return strings.TrimPrefix(key, "/works/") }

// parseLangCode strips the "/languages/" prefix from an OL language key.
func parseLangCode(key string) string { return strings.TrimPrefix(key, "/languages/") }

// parseYear extracts a 4-digit year from strings like "April 2008" or "2008".
func parseYear(s string) int {
	for _, part := range strings.Fields(s) {
		if n, err := strconv.Atoi(part); err == nil && n > 1000 && n < 2200 {
			return n
		}
	}
	return 0
}

// --- SearchByTitle ---

type olSearchDoc struct {
	Key              string   `json:"key"`
	Title            string   `json:"title"`
	AuthorName       []string `json:"author_name"`
	FirstPublishYear int      `json:"first_publish_year"`
	Language         []string `json:"language"`
	CoverI           int      `json:"cover_i"`
}

type olSearchResp struct {
	Docs []olSearchDoc `json:"docs"`
}

func (c *OLClient) SearchByTitle(q string) ([]Candidate, error) {
	path := "/search.json?title=" + url.QueryEscape(q) +
		"&fields=key,title,author_name,first_publish_year,language,cover_i&limit=" +
		strconv.Itoa(searchLimit)
	var resp olSearchResp
	if err := c.get(c.url(path), &resp); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		author := ""
		if len(d.AuthorName) > 0 {
			author = d.AuthorName[0]
		}
		lang := ""
		if len(d.Language) > 0 {
			lang = d.Language[0]
		}
		id := parseWorkID(d.Key)
		out = append(out, Candidate{
			Title:    d.Title,
			Author:   author,
			Year:     d.FirstPublishYear,
			Language: lang,
			WorkID:   id,
			CoverURL: coverURL(c.coversURL, d.CoverI),
			OLURL:    c.baseURL + "/works/" + id,
		})
	}
	return out, nil
}

// --- AuthorSearch ---

type olAuthorDoc struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	WorkCount int    `json:"work_count"`
}

type olAuthorSearchResp struct {
	Docs []olAuthorDoc `json:"docs"`
}

// olWorksAuthorDoc is a work record used only to extract author identity from
// the general search index, which has better recall for prolific authors than
// the dedicated author-search index.
type olWorksAuthorDoc struct {
	AuthorName []string `json:"author_name"`
	AuthorKey  []string `json:"author_key"`
}

type olWorksAuthorResp struct {
	Docs []olWorksAuthorDoc `json:"docs"`
}

func (c *OLClient) AuthorSearch(q string) ([]Author, error) {
	// Primary: dedicated author index (good for exact matches).
	path := "/search/authors.json?q=" + url.QueryEscape(q) +
		"&limit=" + strconv.Itoa(searchLimit)
	var resp olAuthorSearchResp
	if err := c.get(c.url(path), &resp); err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(resp.Docs))
	out := make([]Author, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		seen[d.Key] = true
		out = append(out, Author{Name: d.Name, OLAuthorID: d.Key, WorkCount: d.WorkCount})
	}

	// Supplemental: works-catalog search by author name — catches prolific
	// authors the author index ranks below obscure exact-match records.
	// Best-effort: a failure here just means fewer suggestions.
	var wresp olWorksAuthorResp
	_ = c.get(c.url("/search.json?author="+url.QueryEscape(q)+
		"&fields=author_name,author_key&limit=50"), &wresp)

	qLow := strings.ToLower(q)
	for _, d := range wresp.Docs {
		for i := range d.AuthorKey {
			if i >= len(d.AuthorName) {
				break
			}
			if !strings.Contains(strings.ToLower(d.AuthorName[i]), qLow) {
				continue // skip co-authors whose name doesn't match the query
			}
			key := strings.TrimPrefix(d.AuthorKey[i], "/authors/")
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Author{Name: d.AuthorName[i], OLAuthorID: key})
		}
	}
	return out, nil
}

// --- AuthorWorks ---

func (c *OLClient) AuthorWorks(authorID string) ([]Work, error) {
	// Solr q=author_key:OL\d+A is the correct way to filter by author in OL's
	// search index — the index has cover_i and first_publish_year on most
	// records, unlike /authors/{id}/works.json which omits both. Direct
	// author_key= or /authors/ path forms are not accepted by the API.
	path := "/search.json?q=" + url.QueryEscape("author_key:"+authorID) +
		"&fields=key,title,first_publish_year,cover_i&limit=" + strconv.Itoa(worksLimit)
	var resp olSearchResp
	if err := c.get(c.url(path), &resp); err != nil {
		return nil, err
	}
	out := make([]Work, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		cover := ""
		if d.CoverI > 0 {
			cover = fmt.Sprintf("%s/b/id/%d-M.jpg", c.coversURL, d.CoverI)
		}
		out = append(out, Work{
			WorkID:       parseWorkID(d.Key),
			Title:        d.Title,
			FirstPubYear: d.FirstPublishYear,
			CoverURL:     cover,
		})
	}
	return out, nil
}

// --- WorkDetail ---

type olWorkDetail struct {
	Title            string `json:"title"`
	FirstPublishDate string `json:"first_publish_date"`
}

type olEditionEntry struct {
	Languages []struct {
		Key string `json:"key"`
	} `json:"languages"`
	Covers []int `json:"covers"`
}

type olEditionsResp struct {
	Entries []olEditionEntry `json:"entries"`
}

func (c *OLClient) WorkDetail(id string) (Work, error) {
	var detail olWorkDetail
	if err := c.get(c.url("/works/"+id+".json"), &detail); err != nil {
		return Work{}, err
	}
	var edResp olEditionsResp
	if err := c.get(c.url("/works/"+id+"/editions.json?limit="+strconv.Itoa(editionsLimit)), &edResp); err != nil {
		return Work{}, err
	}
	eds := make([]Edition, 0, len(edResp.Entries))
	for _, e := range edResp.Entries {
		lang := ""
		if len(e.Languages) > 0 {
			lang = parseLangCode(e.Languages[0].Key)
		}
		coverID := 0
		if len(e.Covers) > 0 {
			coverID = e.Covers[0]
		}
		eds = append(eds, Edition{
			Language: lang,
			CoverURL: coverURL(c.coversURL, coverID),
		})
	}
	return Work{
		WorkID:       id,
		Title:        detail.Title,
		FirstPubYear: parseYear(detail.FirstPublishDate),
		Editions:     eds,
	}, nil
}
