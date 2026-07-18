// Per-volume jnovels lookup for the LN volume-note backfill (#90).
//
// A jnovels series page lists its volumes as bare download links, not links to
// per-volume posts — so backfilling each volume's own cover + description means
// running one jnovels *search* per volume and picking the post for that exact
// volume. This file carries the pure matching logic (title-confidence gate keyed
// on the series title, exact volume-number match) plus the Client method that
// searches + scrapes a single volume.
package scraper

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
)

// ErrNoVolumeMatch is returned by VolumeData when no jnovels post confidently
// matches the requested (series, volume). The backfill treats it as a clean miss
// (an "incomplete" placeholder note), distinct from a network/scrape failure.
var ErrNoVolumeMatch = errors.New("no confident jnovels match for this volume")

// apostropheReplacer folds the smart/curly apostrophe variants (and a stray
// backtick/acute accent) down to a plain ASCII apostrophe. jnovels' own search
// chokes on curly apostrophes, and a series title copied from a scraped page
// often carries them — so both the query and the confidence comparison run
// through this first (issue #90).
var apostropheReplacer = strings.NewReplacer(
	"’", "'", // ’ right single quote
	"‘", "'", // ‘ left single quote
	"ʼ", "'", // ʼ modifier letter apostrophe
	"´", "'", // ´ acute accent
	"`", "'", // backtick
)

// NormalizeApostrophes replaces curly/variant apostrophes with a plain ASCII
// one. Exported so the backfill can normalize a series title before searching.
func NormalizeApostrophes(s string) string { return apostropheReplacer.Replace(s) }

// searchQueryText reduces a title to a jnovels-search-friendly query: apostrophes
// normalized, then every character that isn't a letter, digit, space, or
// apostrophe replaced with a space (runs collapsed). jnovels' WordPress search
// mishandles punctuation — a '?' or ',' in the query (as a localized alias like
// "So I'm a Spider, So What?" carries) skews the results right off the wanted
// volume — so stripping them matches the clean word-only query that works.
func searchQueryText(s string) string {
	s = NormalizeApostrophes(s)
	var b strings.Builder
	for _, r := range s {
		if r == '\'' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// volumeNumber pulls the volume number out of a (cleaned) post title like
// "Kumo Desu ga Nani ka Volume 7" → 7, or 0 when the title carries none (an
// aggregate series page).
func volumeNumber(title string) int {
	m := VolumeRE.FindStringSubmatch(title)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// VolumeMatch picks, from ranked search results, the jnovels post for a specific
// volume of a series. Only posts carrying exactly "Volume <volume>" are
// candidates (this alone drops the aggregate page, "Top Light Novels", and
// wrong-volume hits).
//
// Among those, a candidate whose series portion (title minus the trailing volume)
// shares at least two-thirds of the series' word tokens wins — the confident path
// that handles romaji-titled and subtitled posts ("Who Killed the Hero?: Tale of
// the Prophecy Volume 2"). When *none* overlaps, jnovels commonly lists a volume
// under a different localized title than the series page ("Kumo Desu ga Nani ka"
// → "So I'm a Spider, So What? Volume 16"); jnovels' own search is alias-aware and
// still returns it, so a *single* unambiguous exact-volume candidate is trusted on
// the strength of that. Two or more non-overlapping candidates are ambiguous and
// rejected (ok=false) rather than guessed. Pure + deterministic.
func VolumeMatch(results []SearchResult, series string, volume int) (SearchResult, bool) {
	want := searchTokens(NormalizeApostrophes(series))
	if len(want) == 0 {
		return SearchResult{}, false
	}
	var candidates []SearchResult
	var best SearchResult
	bestOverlap := 0
	for _, r := range results {
		if volumeNumber(r.Title) != volume {
			continue
		}
		candidates = append(candidates, r)
		cand := searchTokens(NormalizeApostrophes(seriesTitle(r.Title)))
		overlap := 0
		for tok := range want {
			if cand[tok] {
				overlap++
			}
		}
		if overlap > bestOverlap {
			bestOverlap, best = overlap, r
		}
	}
	// Confident title overlap (≥2/3 of the series tokens) → the best-covered post.
	if bestOverlap*3 >= len(want)*2 {
		return best, true
	}
	// No title overlap, but exactly one exact-volume post → trust alias-aware search.
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return SearchResult{}, false
}

// VolumeData searches jnovels for one volume of a series and scrapes the matched
// post for its cover + description. The series title is apostrophe-normalized
// before searching (jnovels' search dislikes curly apostrophes). It returns the
// scraped data, the post URL, and the series title of the matched post — its
// localized name, which is often different from the series page's ("Kumo Desu ga
// Nani ka" → "So I'm a Spider, So What?") and lets the caller retry sibling
// volumes jnovels' search wouldn't surface under the original title (#90). Returns
// ErrNoVolumeMatch when nothing clears the confidence gate (a clean miss) —
// distinct from a network/scrape error.
func (c *Client) VolumeData(series string, volume int, rl Rules) (nd NovelData, url, postSeries string, err error) {
	query := searchQueryText(series) + " Volume " + strconv.Itoa(volume)
	results, err := c.SearchTitle(query)
	if err != nil {
		return NovelData{}, "", "", err
	}
	m, ok := VolumeMatch(results, series, volume)
	if !ok {
		return NovelData{}, "", "", ErrNoVolumeMatch
	}
	postSeries = seriesTitle(m.Title)
	nd, err = c.NovelData(m.URL, rl)
	if err != nil {
		return NovelData{}, m.URL, postSeries, err
	}
	return nd, m.URL, postSeries, nil
}

// SameSeries reports whether two series titles name the same series, by token
// overlap (case- and apostrophe-insensitive). Used to tell a localized alias
// title ("So I'm a Spider, So What?") apart from the series page's own name so the
// backfill can retry misses under the alias.
func SameSeries(a, b string) bool {
	ta := searchTokens(NormalizeApostrophes(a))
	tb := searchTokens(NormalizeApostrophes(b))
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	overlap := 0
	for t := range ta {
		if tb[t] {
			overlap++
		}
	}
	min := len(ta)
	if len(tb) < min {
		min = len(tb)
	}
	return overlap*2 >= min // share at least half of the smaller title's tokens
}
