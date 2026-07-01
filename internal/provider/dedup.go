package provider

import (
	"regexp"
	"sort"
	"strings"
)

// Matcher resolves an OL work to the Goodreads work cluster it belongs to, by
// ISBN, validating the resolved book's author against `author`. *GRClient
// implements it; tests inject a fake. A miss returns a GRMatch with Found=false.
// Kept tiny so the clustering logic below stays a pure function that can be
// exercised offline against a table of fixtures.
type Matcher interface {
	MatchWork(title, author string, isbns []string) GRMatch
}

// clusterDropRE matches box-sets, bundles, omnibuses, and special/collector
// editions — noise in a baseline picker, where you watch for the next *novel*.
// Mirrors the picker's old client-side filter so server clustering produces the
// same survivor set (the JS filter now becomes a redundant safety net).
var clusterDropRE = regexp.MustCompile(`(?i)\b(limited|rare|lettered|signed|numbered|collector'?s?|deluxe|anniversary|illustrated|trade\s+paperback|box\s*set|boxset|bundle|anthology|omnibus|collection|\d+[\s-]*book|books?\s+\d+|\d+[\s-]*novel)\b`)

var (
	byCreditRE      = regexp.MustCompile(`(?i)\s+by\s+.*$`)
	afterDashRE     = regexp.MustCompile(`\s*[-–—(].*$`)
	leadingArticle  = regexp.MustCompile(`(?i)^(the|a|an)\s+`)
	collapseSpaceRE = regexp.MustCompile(`\s+`)
)

// normTitle collapses English title variants to one key: lowercased, with a
// trailing " by <author>" credit, a subtitle/series/edition suffix after a
// dash/paren, and a leading article stripped. It cannot merge cross-language
// translations or regional retitles ("Painted Man" vs "Warded Man") — those
// share no string; that's what the Goodreads pass is for.
func normTitle(t string) string {
	s := strings.ToLower(t)
	s = byCreditRE.ReplaceAllString(s, "")
	s = afterDashRE.ReplaceAllString(s, "")
	s = leadingArticle.ReplaceAllString(s, "")
	s = collapseSpaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// ClusterWorks collapses an author's OL works that refer to the same logical book
// — across translations, regional titles and reissues — into a single canonical
// entry, and backfills covers from Goodreads. OL files each translation/edition
// as a separate work with no shared identifier, so title-normalization alone
// leaves "The Warded Man", "Malowany człowiek" and "El hombre marcado" as three
// tiles. Two passes:
//
//  1. title-normalization merges the easy English variants and drops box-set
//     noise — cheap and offline.
//  2. for each survivor that carries an ISBN, ask the Matcher for the Goodreads
//     work id every edition of that book shares, and merge survivors that report
//     the same id. A missing cover is backfilled from the match. Bounded by
//     maxLookups so a prolific author can't trigger an unbounded scrape — past
//     the budget (and for any work with no ISBN) the survivor keeps its
//     title-norm grouping. Not every translation merges: Goodreads itself files
//     some (e.g. Polish) as a separate work, so those legitimately stay distinct.
//
// Within a cluster the canonical entry is the English-titled, earliest-year work
// (the original), carrying the best available cover. A nil matcher (Goodreads
// disabled or unavailable) yields the pass-1 result, so the picker degrades to
// the prior title-normalization behaviour rather than failing.
func ClusterWorks(works []Work, author string, m Matcher, maxLookups int) []Work {
	survivors := normMerge(works)
	if m == nil {
		return sortNewestFirst(survivors)
	}

	clusters := map[string]*Work{}
	var order []string
	lookups := 0
	for i := range survivors {
		w := survivors[i]
		key := "norm:" + normTitle(w.Title)
		if lookups < maxLookups && len(w.ISBNs) > 0 {
			gm := m.MatchWork(w.Title, author, w.ISBNs)
			lookups++
			if gm.Found {
				if w.CoverURL == "" && gm.CoverURL != "" {
					w.CoverURL = gm.CoverURL
				}
				if gm.WorkID != "" {
					key = "gr:" + gm.WorkID
				}
			}
		}
		if existing, ok := clusters[key]; ok {
			*existing = mergeWork(*existing, w)
		} else {
			ww := w
			clusters[key] = &ww
			order = append(order, key)
		}
	}

	out := make([]Work, 0, len(order))
	for _, k := range order {
		out = append(out, *clusters[k])
	}
	return sortNewestFirst(out)
}

// normMerge runs pass 1: drop noise, then keep the earliest-year work per
// normalized title (the most canonical printing). Order of input is preserved
// for ties via the stable year sort.
func normMerge(works []Work) []Work {
	sorted := append([]Work(nil), works...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return yearOrMax(sorted[i].FirstPubYear) < yearOrMax(sorted[j].FirstPubYear)
	})
	seen := map[string]int{} // norm key -> index in out
	var out []Work
	for _, w := range sorted {
		if clusterDropRE.MatchString(w.Title) {
			continue
		}
		k := normTitle(w.Title)
		if k == "" {
			continue
		}
		if idx, ok := seen[k]; ok {
			out[idx] = mergeWork(out[idx], w)
			continue
		}
		seen[k] = len(out)
		out = append(out, w)
	}
	return out
}

// mergeWork folds b into a, returning the entry that should represent the
// cluster: the more-canonical title (English over a non-ASCII translation, then
// earliest year), enriched with whichever cover and year are present.
func mergeWork(a, b Work) Work {
	canon, other := a, b
	if preferCanon(b, a) {
		canon, other = b, a
	}
	if canon.CoverURL == "" && other.CoverURL != "" {
		canon.CoverURL = other.CoverURL
	}
	if canon.FirstPubYear == 0 {
		canon.FirstPubYear = other.FirstPubYear
	}
	if canon.Language == "" && other.Language != "" {
		canon.Language = other.Language
	}
	return canon
}

// preferCanon reports whether cand is a better cluster representative than cur:
// an English (ASCII) title beats a transliterated/accented translation; among
// titles of the same script the earlier original wins; a cover breaks the tie.
func preferCanon(cand, cur Work) bool {
	if ca, cu := isASCII(cand.Title), isASCII(cur.Title); ca != cu {
		return ca
	}
	cy, uy := yearOrMax(cand.FirstPubYear), yearOrMax(cur.FirstPubYear)
	if cy != uy {
		return cy < uy
	}
	return cand.CoverURL != "" && cur.CoverURL == ""
}

func sortNewestFirst(works []Work) []Work {
	sort.SliceStable(works, func(i, j int) bool {
		return works[i].FirstPubYear > works[j].FirstPubYear
	})
	return works
}

func yearOrMax(y int) int {
	if y <= 0 {
		return 1 << 30
	}
	return y
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}
