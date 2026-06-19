// Package checker orchestrates a check run (read-only: it reports what WOULD
// update, writing nothing). Per-entry scrape rules are resolved via a callback.
package checker

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"bookwatch/internal/scraper"
	"bookwatch/internal/vault"
)

// defaultConcurrency is the number of parallel fetches when
// BOOKWATCH_CHECK_CONCURRENCY isn't set.
const defaultConcurrency = 6

// concurrency reads the per-check parallelism from the environment, falling back
// to defaultConcurrency. Read at call time so tests can override it.
func concurrency() int {
	if v := os.Getenv("BOOKWATCH_CHECK_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultConcurrency
}

// Result is the outcome for one entry.
type Result struct {
	Entry      vault.Entry
	Latest     int
	Count      int  // volume-list items found; 0 (with no error) usually means the rules broke
	HasNew     bool // Latest > Entry.Volumes
	Suspicious bool // scrape succeeded but looks wrong — likely broken rules (see Check)
	Err        error
}

// Check fetches each entry's latest volume and flags new ones, using a bounded
// pool of concurrent fetches (BOOKWATCH_CHECK_CONCURRENCY, default 6). resolve
// maps a URL to its scrape rules (e.g. sources.Resolver.For) and is called
// concurrently — it must be safe for concurrent use (Resolver.For only reads).
// Results keep input order (each worker writes its own slot); progress reports a
// monotonic completed count.
func Check(
	entries []vault.Entry,
	sc *scraper.Client,
	resolve func(string) scraper.Rules,
	progress func(i, total int, title string),
) []Result {
	total := len(entries)
	results := make([]Result, total)
	if total == 0 {
		return results
	}

	limit := concurrency()
	if limit > total {
		limit = total
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var done int32

	for i := range entries {
		wg.Add(1)
		sem <- struct{}{} // blocks once `limit` fetches are in flight
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			e := entries[i]
			latest, count, err := sc.LatestVolume(e.Link, resolve(e.Link))
			hasNew := err == nil && latest > e.Volumes
			// A 200 that yields no list items (count==0) or fewer volumes than we
			// already recorded (a regression) almost always means the site's
			// layout changed and the selectors no longer match — flag it rather
			// than silently report "no update".
			suspicious := err == nil && !hasNew && (count == 0 || latest < e.Volumes)
			results[i] = Result{
				Entry:      e,
				Latest:     latest,
				Count:      count,
				HasNew:     hasNew,
				Suspicious: suspicious,
				Err:        err,
			}
			if progress != nil {
				progress(int(atomic.AddInt32(&done, 1)), total, e.Title)
			}
		}(i)
	}
	wg.Wait()
	return results
}
