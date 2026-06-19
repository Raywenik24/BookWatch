// Package service is the orchestration shared by the CLI, the HTTP API, and
// the scheduler: scan → check → (optionally) write → (optionally) record.
package service

import (
	"errors"
	"fmt"
	"io/fs"
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
	Checked    int          `json:"checked"`
	Updated    int          `json:"updated"`
	Errors     int          `json:"errors"`
	Suspicious int          `json:"suspicious"` // scraped OK but looked wrong (broken rules?)
	Updates    []UpdateInfo `json:"updates"`
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
			// A scrape that succeeded but read no list items / fewer volumes than
			// recorded is logged so a silently-broken source can't masquerade as
			// "no update". The book's volumes are left as-is (we don't trust the
			// bad read).
			if r.Suspicious {
				sum.Suspicious++
				if st != nil {
					st.LogEvent("anomaly", fmt.Sprintf(
						"%q: scrape read %d volume(s) (have %d) — the source's rules may be broken",
						r.Entry.Title, r.Latest, r.Entry.Volumes))
				}
			}
			if st != nil {
				if _, e := st.UpsertBook(r.Entry.Title, r.Entry.Link, r.Entry.Path, r.Entry.Volumes, r.Entry.Cover); e != nil {
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
			// Detect-only runs leave the book's volume count untouched and log the
			// bump as pending; only an explicit apply (write) bumps it + stamps it.
			vol := r.Entry.Volumes
			if wrote {
				vol = r.Latest
			}
			bookID, e := st.UpsertBook(r.Entry.Title, r.Entry.Link, r.Entry.Path, vol, r.Entry.Cover)
			if e != nil {
				return sum, e
			}
			upID, e := st.UpsertPendingUpdate(bookID, r.Entry.Volumes, r.Latest, r.Entry.Link)
			if e != nil {
				return sum, e
			}
			if wrote {
				if e := st.MarkApplied(upID, bookID, r.Latest); e != nil {
					return sum, e
				}
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
			if _, err := os.Stat(b.Path); err == nil || !errors.Is(err, fs.ErrNotExist) {
				// Keep the book if the note is on disk OR if we can't confirm it's
				// gone: a permission/IO/OneDrive-sync hiccup must not trigger a
				// prune. Only a definite "does not exist" drops the row.
				continue
			}
			if e := st.DeleteBook(b.ID); e != nil {
				return sum, e
			}
			st.LogEvent("prune", fmt.Sprintf("Auto-pruned stale book %q (note gone from disk)", b.Title))
		}
	}

	if st != nil {
		summary := fmt.Sprintf("%d notes, %d updates, %d errors, %d suspicious", sum.Checked, sum.Updated, sum.Errors, sum.Suspicious)
		if e := st.FinishRun(runID, sum.Checked, sum.Updated, sum.Errors, summary); e != nil {
			return sum, e
		}
	}
	return sum, nil
}

// ApplyResult is the outcome of applying pending updates to the vault.
type ApplyResult struct {
	Applied int          `json:"applied"`
	Failed  int          `json:"failed"`
	Updates []UpdateInfo `json:"updates"`
}

// ApplyPending writes every pending update's stored volume count to its vault
// note (vault.UpdateVolumes), bumps the book, and stamps the update applied. It
// applies the LAST check's numbers — it does not re-scrape.
func ApplyPending(st *store.Store, today string) (ApplyResult, error) {
	pending, err := st.ListPending()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("list pending: %w", err)
	}
	var res ApplyResult
	for _, p := range pending {
		if e := vault.UpdateVolumes(p.Path, p.NewVolumes, today); e != nil {
			res.Failed++
			continue
		}
		if e := st.MarkApplied(p.ID, p.BookID, p.NewVolumes); e != nil {
			return res, e
		}
		res.Applied++
		res.Updates = append(res.Updates, UpdateInfo{
			Title: p.Title, Link: p.Link,
			OldVolumes: p.OldVolumes, NewVolumes: p.NewVolumes, Wrote: true,
		})
	}
	return res, nil
}
