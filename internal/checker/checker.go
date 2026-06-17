// Package checker orchestrates a check run (read-only: it reports what WOULD
// update, writing nothing). Per-entry scrape rules are resolved via a callback.
package checker

import (
	"pagewatch/internal/scraper"
	"pagewatch/internal/vault"
)

// Result is the outcome for one entry.
type Result struct {
	Entry  vault.Entry
	Latest int
	HasNew bool // Latest > Entry.Volumes
	Err    error
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
		latest, _, err := sc.LatestVolume(e.Link, resolve(e.Link))
		results = append(results, Result{
			Entry:  e,
			Latest: latest,
			HasNew: err == nil && latest > e.Volumes,
			Err:    err,
		})
	}
	return results
}
