// Package provider queries an external catalog (OpenLibrary) by author/title
// and returns structured bibliography data. It sits beside the scraping path —
// the LN scraper is untouched.
package provider

// Candidate is one title hit from a catalog search.
type Candidate struct {
	Title     string
	Author    string
	AuthorKey string // e.g. "OL1234A" — empty when the source has no author_key
	Year      int
	Language  string // e.g. "eng"
	WorkID    string // e.g. "OL1234W"
	CoverURL  string
	OLURL     string
}

// Author is one result from an author search.
type Author struct {
	Name       string
	OLAuthorID string // e.g. "OL1234A"
	WorkCount  int
}

// Edition is one edition of a work with its title, language and cover. Title
// matters because a work's display Title is itself just one edition's title
// (often the English one) — a translated edition can carry a completely
// different title, e.g. Work "Season of Storms" has a Polish edition titled
// "Sezon Burz" (#45).
type Edition struct {
	Title    string
	Language string
	CoverURL string
}

// Work is a catalog work record.
type Work struct {
	WorkID       string
	Title        string
	FirstPubYear int
	Language     string   // OL language code, e.g. "eng"; empty if unknown
	Languages    []string // every language OL has an edition of this work in (#45) — Language is just Languages[0], or "" if empty
	CoverURL     string
	// CoverUnverified marks a cover borrowed via the opt-in unsafe ISBN match —
	// the Goodreads book resolved by ISBN but its author didn't match, so the
	// safe passes' author guard would normally reject it. The picker flags such
	// a tile so the user knows the cover is a best-guess, not verified (#41).
	CoverUnverified bool
	Description     string
	Editions        []Edition
	ISBNs           []string // a few edition ISBNs — the cross-source key for Goodreads clustering (#40)
}

// Provider is the catalog lookup interface.
type Provider interface {
	SearchByTitle(q string) ([]Candidate, error)
	AuthorSearch(q string) ([]Author, error)
	AuthorWorks(authorID string) ([]Work, error)
	WorkDetail(workID string) (Work, error)
	// WorkEditions returns per-edition language/cover data for a work — the
	// accurate source for a work's language, unlike the aggregated tag
	// AuthorWorks returns (#45).
	WorkEditions(workID string) ([]Edition, error)
	// WorkByID resolves a single work (e.g. from a pasted openlibrary.org/works/
	// URL) into a Candidate, without going through title search.
	WorkByID(workID string) (Candidate, error)
}

// MajorityLanguage returns the most common non-empty language among a
// work's editions — a single work can have translated editions mixed in
// under the same OL work record, so majority vote beats picking edition[0].
func MajorityLanguage(eds []Edition) string {
	counts := make(map[string]int, len(eds))
	order := make([]string, 0, len(eds))
	for _, e := range eds {
		if e.Language == "" {
			continue
		}
		if counts[e.Language] == 0 {
			order = append(order, e.Language)
		}
		counts[e.Language]++
	}
	best, bestN := "", 0
	for _, lang := range order {
		if counts[lang] > bestN {
			best, bestN = lang, counts[lang]
		}
	}
	return best
}

// FindEdition returns the first edition tagged with lang — the accurate way
// to check whether a work actually has an edition in a given catalog
// language (and get its real title/cover), since a work's own Language field
// is an unreliable aggregate that can miss or misreport translations (#45).
func FindEdition(eds []Edition, lang string) (Edition, bool) {
	for _, e := range eds {
		if e.Language == lang {
			return e, true
		}
	}
	return Edition{}, false
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
