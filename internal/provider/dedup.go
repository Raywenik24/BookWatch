package provider

import (
	"regexp"
	"sort"
	"strings"
)

// Matcher resolves an OL work to the Goodreads work cluster it belongs to, by
// ISBN, validating the resolved book's author against `author`. *GRClient
// implements it; tests inject a fake. A miss returns a Match with Found=false.
// Kept tiny so the clustering logic below stays a pure function that can be
// exercised offline against a table of fixtures.
type Matcher interface {
	MatchWork(title, author string, isbns []string) Match
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
// entry, and backfills missing covers. OL files each translation/edition as a
// separate work with no shared identifier, so title-normalization alone leaves
// "The Warded Man", "Malowany człowiek" and "El hombre marcado" as three tiles.
// Three passes:
//
//  1. title-normalization merges the easy English variants and drops box-set
//     noise — cheap and offline.
//  2. the Goodreads matcher (gr): for each survivor with an ISBN, resolve the
//     Goodreads work id every edition shares and merge survivors reporting the
//     same id. Handles English/French/Spanish/Portuguese clusters.
//  3. the Lubimyczytać matcher (lc): for each *Polish-titled* survivor left, do
//     the same by Polish title — Goodreads files Polish editions as separate
//     works and OL tags them language:null, so pass 2 can't collapse them (#43).
//
// Each matcher pass backfills a missing cover from its match and has its own
// lookup budget, so a prolific author can't trigger an unbounded scrape — past
// the budget (or when a survivor doesn't qualify for a pass) the survivor keeps
// its current grouping. The two budgets are separate because the passes cost
// very differently: Goodreads is one fetch per ISBN, Lubimyczytać is a title
// search plus a book-page fetch, and a Polish author routes almost every
// survivor to it — so lcBudget is kept small to bound picker latency. The LC
// pass also spends its budget coverless-works-first, so the limited lookups go
// where they show (backfilling a missing cover) rather than on works already
// covered. Within a cluster the canonical entry is the English-titled,
// earliest-year work, carrying the best available cover. A nil matcher (source
// disabled or unavailable) simply skips its pass, so the picker degrades
// gracefully rather than failing.
func ClusterWorks(works []Work, author string, gr, lc Matcher, grBudget, lcBudget int) []Work {
	survivors := normMerge(works)
	survivors = matcherPass(survivors, author, gr, grBudget, hasISBN, nil)
	survivors = matcherPass(survivors, author, lc, lcBudget, looksPolish, coverlessFirst)
	return sortNewestFirst(survivors)
}

// matcherPass merges the survivors that resolve to the same matcher work id,
// backfilling missing covers. Only works passing `want` are looked up (each pass
// targets a different subset — ISBN-bearing for Goodreads, Polish-titled for
// Lubimyczytać); the rest keep their title-norm grouping. At most `budget`
// lookups run, in `priority` order when given (lower value first) so a small
// budget spends where it matters most; a nil priority keeps document order. A
// nil matcher returns the input untouched (pass skipped). The id key is
// namespaced ("id:") so a raw Goodreads work id and an LC series key can't
// collide across passes.
func matcherPass(works []Work, author string, m Matcher, budget int, want func(Work) bool, priority func(Work) int) []Work {
	if m == nil {
		return works
	}

	// Choose which works to look up, in priority order, up to the budget.
	order := make([]int, 0, len(works))
	for i := range works {
		if want(works[i]) {
			order = append(order, i)
		}
	}
	if priority != nil {
		sort.SliceStable(order, func(a, b int) bool {
			return priority(works[order[a]]) < priority(works[order[b]])
		})
	}
	resolved := map[int]Match{}
	for n, i := range order {
		if n >= budget {
			break
		}
		if gm := m.MatchWork(works[i].Title, author, works[i].ISBNs); gm.Found {
			resolved[i] = gm
		}
	}

	// Cluster in original document order, applying whatever was resolved.
	clusters := map[string]*Work{}
	var keys []string
	for i := range works {
		w := works[i]
		key := "norm:" + normTitle(w.Title)
		if gm, ok := resolved[i]; ok {
			if w.CoverURL == "" && gm.CoverURL != "" {
				w.CoverURL = gm.CoverURL
			}
			if gm.WorkID != "" {
				key = "id:" + gm.WorkID
			}
		}
		if existing, ok := clusters[key]; ok {
			*existing = mergeWork(*existing, w)
		} else {
			ww := w
			clusters[key] = &ww
			keys = append(keys, key)
		}
	}

	out := make([]Work, 0, len(keys))
	for _, k := range keys {
		out = append(out, *clusters[k])
	}
	return out
}

// hasISBN gates the Goodreads pass: only works carrying an ISBN can be resolved
// through the /book/isbn entry point.
func hasISBN(w Work) bool { return len(w.ISBNs) > 0 }

// coverlessFirst orders the Lubimyczytać pass so works missing a cover are looked
// up before ones that already have one — the small LC budget goes to the tiles
// where a backfilled cover is actually visible.
func coverlessFirst(w Work) int {
	if w.CoverURL == "" {
		return 0
	}
	return 1
}

// polishChars are the letters distinctive to Polish; their presence marks a title
// as a Polish edition worth routing to the Lubimyczytać (title-keyed) pass. ó is
// left out on purpose — it also appears in Spanish/French, which pass 2 handles.
const polishChars = "ąćęłńśźżĄĆĘŁŃŚŹŻ"

// looksPolish gates the Lubimyczytać pass: a title carrying a distinctly-Polish
// letter. Deliberately narrow — the pass is a title search, so it should only
// fire on titles that are actually Polish, not every accented foreign survivor.
func looksPolish(w Work) bool {
	return strings.ContainsAny(w.Title, polishChars)
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
