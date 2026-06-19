// Package checker orchestrates a check run (read-only: it reports what WOULD
// update, writing nothing). Per-entry scrape rules are resolved via a callback.
package checker

import (
	"bookwatch/internal/scraper"
	"bookwatch/internal/vault"
)

// Result is the outcome for one entry.
type Result struct {
	Entry      vault.Entry
	Latest     int
	Count      int  // volume-list items found; 0 (with no error) usually means the rules broke
	HasNew     bool // Latest > Entry.Volumes
	Suspicious bool // scrape succeeded but looks wrong — likely broken rules (see Check)
	Err        error
}

// Check fetches each entry's latest volume and flags new ones. resolve maps a
// URL to its scrape rules (e.g. sources.Resolver.For). Sequential for
// deterministic, comparable output.
func Check(
	entries []vault.Entry,
	sc *scraper.Client,
	resolve func(string) scraper.Rules,
	progress func(i, total int, title string),
) []Result {
	results := make([]Result, 0, len(entries))
	for i, e := range entries {
		if progress != nil {
			progress(i+1, len(entries), e.Title)
		}
		latest, count, err := sc.LatestVolume(e.Link, resolve(e.Link))
		hasNew := err == nil && latest > e.Volumes
		// A 200 that yields no list items (count==0) or fewer volumes than we
		// already recorded (a regression) almost always means the site's layout
		// changed and the selectors no longer match — flag it rather than
		// silently report "no update".
		suspicious := err == nil && !hasNew && (count == 0 || latest < e.Volumes)
		results = append(results, Result{
			Entry:      e,
			Latest:     latest,
			Count:      count,
			HasNew:     hasNew,
			Suspicious: suspicious,
			Err:        err,
		})
	}
	return results
}
