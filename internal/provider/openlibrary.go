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
	AuthorKey        []string `json:"author_key"`
	FirstPublishYear int      `json:"first_publish_year"`
	Language         []string `json:"language"`
	CoverI           int      `json:"cover_i"`
	ISBN             []string `json:"isbn"`
}

type olSearchResp struct {
	Docs []olSearchDoc `json:"docs"`
}

func (c *OLClient) SearchByTitle(q string) ([]Candidate, error) {
	path := "/search.json?title=" + url.QueryEscape(q) +
		"&fields=key,title,author_name,author_key,first_publish_year,language,cover_i&limit=" +
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
		authorKey := ""
		if len(d.AuthorKey) > 0 {
			authorKey = strings.TrimPrefix(d.AuthorKey[0], "/authors/")
		}
		lang := ""
		if len(d.Language) > 0 {
			lang = d.Language[0]
		}
		id := parseWorkID(d.Key)
		out = append(out, Candidate{
			Title:     d.Title,
			Author:    author,
			AuthorKey: authorKey,
			Year:      d.FirstPublishYear,
			Language:  lang,
			WorkID:    id,
			CoverURL:  coverURL(c.coversURL, d.CoverI),
			OLURL:     c.baseURL + "/works/" + id,
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
	// isbn comes back as the union of every edition's ISBNs; we keep a handful as
	// the cross-source key for Goodreads work-clustering (#40).
	path := "/search.json?q=" + url.QueryEscape("author_key:"+authorID) +
		"&fields=key,title,first_publish_year,language,cover_i,isbn&limit=" + strconv.Itoa(worksLimit)
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
		// d.Language is every language OL has an edition of this work in, not
		// just the display edition's — a work titled "Season of Storms" can
		// still have a "pol" edition ("Sezon Burz") buried in this list even
		// though [0] says "eng" (#45). Keep the whole thing so the picker can
		// check "does this work have my catalog language at all" instead of
		// trusting an arbitrarily-ordered single tag.
		lang := ""
		if len(d.Language) > 0 {
			lang = d.Language[0]
		}
		out = append(out, Work{
			WorkID:       parseWorkID(d.Key),
			Title:        d.Title,
			FirstPubYear: d.FirstPublishYear,
			Language:     lang,
			Languages:    d.Language,
			CoverURL:     cover,
			ISBNs:        capISBNs(d.ISBN),
		})
	}
	return out, nil
}

// maxISBNsPerWork caps how many of a work's edition ISBNs we keep — the Goodreads
// match stops at the first that resolves, so a long list only adds dead lookups.
const maxISBNsPerWork = 5

func capISBNs(isbns []string) []string {
	if len(isbns) <= maxISBNsPerWork {
		return isbns
	}
	return isbns[:maxISBNsPerWork]
}

// --- WorkDetail ---

// olDescription unmarshals OL's "description" field, which is either a bare
// string or a {"type":"/type/text","value":"..."} object depending on the
// record.
type olDescription struct{ Value string }

func (d *olDescription) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		d.Value = s
		return nil
	}
	var obj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	d.Value = obj.Value
	return nil
}

type olWorkDetail struct {
	Title            string        `json:"title"`
	FirstPublishDate string        `json:"first_publish_date"`
	Description      olDescription `json:"description"`
}

type olEditionEntry struct {
	Title     string `json:"title"`
	Languages []struct {
		Key string `json:"key"`
	} `json:"languages"`
	Covers []int    `json:"covers"`
	ISBN13 []string `json:"isbn_13"`
	ISBN10 []string `json:"isbn_10"`
}

type olEditionsResp struct {
	Entries []olEditionEntry `json:"entries"`
}

func (c *OLClient) WorkDetail(id string) (Work, error) {
	var detail olWorkDetail
	if err := c.get(c.url("/works/"+id+".json"), &detail); err != nil {
		return Work{}, err
	}
	edResp, err := c.fetchEditions(id)
	if err != nil {
		return Work{}, err
	}
	// Editions carry their own per-language ISBN (via provider.EditionISBNs)
	// — a sparse translation work record can be missing its author/
	// description even in OL's own index (#42), and the caller needs an
	// ISBN from the *same* language edition to backfill safely; Work.ISBNs
	// mixes every translation's ISBNs together and isn't safe for that.
	return Work{
		WorkID:       id,
		Title:        detail.Title,
		FirstPubYear: parseYear(detail.FirstPublishDate),
		Description:  detail.Description.Value,
		Editions:     editionsFromResp(edResp, c.coversURL),
	}, nil
}

// WorkEditions fetches per-edition language and cover data for a work — the
// accurate source, unlike the search index's per-work "language" field which
// aggregates every translated edition into one arbitrarily-picked tag (#45).
func (c *OLClient) WorkEditions(id string) ([]Edition, error) {
	edResp, err := c.fetchEditions(id)
	if err != nil {
		return nil, err
	}
	return editionsFromResp(edResp, c.coversURL), nil
}

func (c *OLClient) fetchEditions(id string) (olEditionsResp, error) {
	var edResp olEditionsResp
	err := c.get(c.url("/works/"+id+"/editions.json?limit="+strconv.Itoa(editionsLimit)), &edResp)
	return edResp, err
}

func editionsFromResp(edResp olEditionsResp, coversURL string) []Edition {
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
		isbn := ""
		if len(e.ISBN13) > 0 {
			isbn = e.ISBN13[0]
		} else if len(e.ISBN10) > 0 {
			isbn = e.ISBN10[0]
		}
		eds = append(eds, Edition{
			Title:    e.Title,
			Language: lang,
			CoverURL: coverURL(coversURL, coverID),
			ISBN:     isbn,
		})
	}
	return eds
}

// --- WorkByID ---

type olWorkFull struct {
	Title            string `json:"title"`
	FirstPublishDate string `json:"first_publish_date"`
	Covers           []int  `json:"covers"`
	Authors          []struct {
		Author struct {
			Key string `json:"key"`
		} `json:"author"`
	} `json:"authors"`
}

type olAuthorDetail struct {
	Name string `json:"name"`
}

// WorkByID resolves a single work straight from its ID — the path for a
// pasted openlibrary.org/works/OL...W URL, where the user already picked the
// exact work and a title search would be redundant. The author name needs a
// second fetch since /works/{id}.json only carries the author's key.
func (c *OLClient) WorkByID(id string) (Candidate, error) {
	var w olWorkFull
	if err := c.get(c.url("/works/"+id+".json"), &w); err != nil {
		return Candidate{}, err
	}
	author, authorKey := "", ""
	if len(w.Authors) > 0 && w.Authors[0].Author.Key != "" {
		authorKey = strings.TrimPrefix(w.Authors[0].Author.Key, "/authors/")
		var a olAuthorDetail
		if err := c.get(c.url(w.Authors[0].Author.Key+".json"), &a); err == nil {
			author = a.Name
		}
	}
	cover := ""
	if len(w.Covers) > 0 {
		cover = coverURL(c.coversURL, w.Covers[0])
	}
	return Candidate{
		Title:     w.Title,
		Author:    author,
		AuthorKey: authorKey,
		Year:      parseYear(w.FirstPublishDate),
		WorkID:    id,
		CoverURL:  cover,
		OLURL:     c.baseURL + "/works/" + id,
	}, nil
}
