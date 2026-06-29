// Package provider queries an external catalog (OpenLibrary) by author/title
// and returns structured bibliography data. It sits beside the scraping path —
// the LN scraper is untouched.
package provider

// Candidate is one title hit from a catalog search.
type Candidate struct {
	Title    string
	Author   string
	Year     int
	Language string // e.g. "eng"
	WorkID   string // e.g. "OL1234W"
	CoverURL string
	OLURL    string
}

// Author is one result from an author search.
type Author struct {
	Name       string
	OLAuthorID string // e.g. "OL1234A"
	WorkCount  int
}

// Edition is one edition of a work with its language and cover.
type Edition struct {
	Language string
	CoverURL string
}

// Work is a catalog work record.
type Work struct {
	WorkID       string
	Title        string
	FirstPubYear int
	Editions     []Edition
}

// Provider is the catalog lookup interface.
type Provider interface {
	SearchByTitle(q string) ([]Candidate, error)
	AuthorSearch(q string) ([]Author, error)
	AuthorWorks(authorID string) ([]Work, error)
	WorkDetail(workID string) (Work, error)
}

// SelectCover returns the cover URL for the first edition matching lang.
// Falls back to the first edition with any cover, then empty string.
func SelectCover(w Work, lang string) string {
	fallback := ""
	for _, e := range w.Editions {
		if e.CoverURL == "" {
			continue
		}
		if fallback == "" {
			fallback = e.CoverURL
		}
		if e.Language == lang {
			return e.CoverURL
		}
	}
	return fallback
}
