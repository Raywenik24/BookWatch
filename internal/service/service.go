// Package service is the orchestration shared by the CLI, the HTTP API, and
// the scheduler: scan → check → (optionally) write → (optionally) record.
package service

import (
	"fmt"
	"os"

	"bookwatch/internal/checker"
	"bookwatch/internal/scraper"
	"bookwatch/internal/sources"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

// UpdateInfo is one detected new volume.
type UpdateInfo struct {
	Title      string `json:"title"`
	Link       string `json:"link"`
	OldVolumes int    `json:"old_volumes"`
	NewVolumes int    `json:"new_volumes"`
	Wrote      bool   `json:"wrote"`
}

// CheckSummary is the outcome of a run.
type CheckSummary struct {
	Checked int          `json:"checked"`
	Updated int          `json:"updated"`
	Errors  int          `json:"errors"`
	Updates []UpdateInfo `json:"updates"`
}

// RunCheck scans scanRoot, checks each book, optionally writes vault updates,
// and (when st != nil) records the run + updates + books. progress may be nil.
func RunCheck(sc *scraper.Client, st *store.Store, scanRoot string, write bool,
	progress func(i, total int, title string)) (CheckSummary, error) {

	entries, err := vault.Scan(scanRoot)
	if err != nil {
		return CheckSummary{}, fmt.Errorf("scan: %w", err)
	}

	resolver := sources.NewResolver(st)
	results := checker.Check(entries, sc, resolver.For, progress)

	var runID int64
	if st != nil {
		if runID, err = st.StartRun(); err != nil {
			return CheckSummary{}, fmt.Errorf("start run: %w", err)
		}
	}

	today := vault.Today()
	sum := CheckSummary{Checked: len(results)}

	for _, r := range results {
		if r.Err != nil {
			sum.Errors++
			continue
		}
		if !r.HasNew {
			if st != nil {
				if _, e := st.UpsertBook(r.Entry.Title, r.Entry.Link, r.Entry.Path, r.Entry.Volumes); e != nil {
					return sum, e
				}
			}
			continue
		}

		sum.Updated++
		wrote := false
		if write {
			if e := vault.UpdateVolumes(r.Entry.Path, r.Latest, today); e == nil {
				wrote = true
			}
		}
		sum.Updates = append(sum.Updates, UpdateInfo{
			Title: r.Entry.Title, Link: r.Entry.Link,
			OldVolumes: r.Entry.Volumes, NewVolumes: r.Latest, Wrote: wrote,
		})

		if st != nil {
			vol := r.Entry.Volumes
			if wrote {
				vol = r.Latest
			}
			bookID, e := st.UpsertBook(r.Entry.Title, r.Entry.Link, r.Entry.Path, vol)
			if e != nil {
				return sum, e
			}
			if e := st.RecordUpdate(bookID, r.Entry.Volumes, r.Latest, r.Entry.Link); e != nil {
				return sum, e
			}
		}
	}

	// Auto-prune: any tracked book absent from this scan (by link) whose note
	// file is also gone from disk is a stale row → drop it. Vault is the source
	// of truth, so a note that merely moved (still scanned) is never pruned.
	if st != nil {
		seen := make(map[string]bool, len(results))
		for _, r := range results {
			seen[r.Entry.Link] = true
		}
		tracked, e := st.ListBooks()
		if e != nil {
			return sum, e
		}
		for _, b := range tracked {
			if seen[b.Link] {
				continue
			}
			if _, err := os.Stat(b.Path); err == nil {
				continue // note still on disk; leave it tracked
			}
			if e := st.DeleteBook(b.ID); e != nil {
				return sum, e
			}
		}
	}

	if st != nil {
		summary := fmt.Sprintf("%d notes, %d updates, %d errors", sum.Checked, sum.Updated, sum.Errors)
		if e := st.FinishRun(runID, sum.Checked, sum.Updated, sum.Errors, summary); e != nil {
			return sum, e
		}
	}
	return sum, nil
}
