// Package service is the orchestration shared by the CLI, the HTTP API, and
// the scheduler: scan → check → (optionally) write → (optionally) record.
package service

import (
	"fmt"
	"strings"

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

	// Split by kind: only LN entries go through the volume checker.
	var lnEntries, bookEntries []vault.Entry
	for _, e := range entries {
		if e.Kind == "book" {
			bookEntries = append(bookEntries, e)
		} else {
			lnEntries = append(lnEntries, e)
		}
	}

	resolver := sources.NewResolver(st)
	results := checker.Check(lnEntries, sc, resolver.For, progress)

	var runID int64
	if st != nil {
		if runID, err = st.StartRun(); err != nil {
			return CheckSummary{}, fmt.Errorf("start run: %w", err)
		}
	}

	today := vault.Today()
	sum := CheckSummary{Checked: len(lnEntries)}

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
			applyStatusCorrection(&r, st)
			if st != nil {
				rv := entryReadVolumes(r.Entry)
				if _, e := st.UpsertBook(r.Entry.Title, r.Entry.Link, r.Entry.Path, r.Entry.Volumes, r.Entry.Cover, r.Entry.Status, rv, "ln", r.Entry.Author); e != nil {
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

		applyStatusCorrection(&r, st)

		if st != nil {
			// Detect-only runs leave the book's volume count untouched and log the
			// bump as pending; only an explicit apply (write) bumps it + stamps it.
			vol := r.Entry.Volumes
			if wrote {
				vol = r.Latest
			}
			rv := entryReadVolumes(r.Entry)
			bookID, e := st.UpsertBook(r.Entry.Title, r.Entry.Link, r.Entry.Path, vol, r.Entry.Cover, r.Entry.Status, rv, "ln", r.Entry.Author)
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

	// Upsert book entries: no volume check, no status correction.
	if st != nil {
		for _, e := range bookEntries {
			if _, err := st.UpsertBook(e.Title, e.Link, e.Path, 0, e.Cover, e.Status, nil, "book", e.Author); err != nil {
				return sum, err
			}
		}
	}

	// Auto-prune: any tracked entry absent from this scan is a stale row → drop
	// it. The scan is the source of truth: a note not returned by vault.Scan
	// (missing, moved without the tag, or no longer matching the filter) should
	// not remain in the DB.
	if st != nil {
		seen := make(map[string]bool, len(results)+len(bookEntries))
		for _, r := range results {
			seen[r.Entry.Link] = true
		}
		for _, e := range bookEntries {
			seen[e.Link] = true
		}
		tracked, e := st.ListBooks()
		if e != nil {
			return sum, e
		}
		for _, b := range tracked {
			if seen[b.Link] {
				continue
			}
			if e := st.DeleteBook(b.ID); e != nil {
				return sum, e
			}
			st.LogEvent("prune", fmt.Sprintf("Auto-pruned %q (not in scan)", b.Title))
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

// determineStatusCorrection returns the corrected status for an entry given
// the check result, or "" if no correction is needed. Dropped is never touched.
func determineStatusCorrection(e vault.Entry, hasNew bool) string {
	if strings.EqualFold(e.Status, "Dropped") {
		return ""
	}
	readVols := e.ReadVolumes // 0 when !HasReadVolumes
	if hasNew && strings.EqualFold(e.Status, "Completed") {
		return "Queue"
	}
	if readVols < e.Volumes && strings.EqualFold(e.Status, "Completed") {
		return "Queue"
	}
	if !hasNew && e.Volumes > 0 && readVols == e.Volumes && strings.EqualFold(e.Status, "Queue") {
		return "Completed"
	}
	return ""
}

// applyStatusCorrection writes the corrected status to the vault note when the
// rules fire, then mutates r.Entry.Status so subsequent store writes use it.
// Vault writes are not gated by the run's write flag — corrections are immediate.
func applyStatusCorrection(r *checker.Result, st *store.Store) {
	newStatus := determineStatusCorrection(r.Entry, r.HasNew)
	if newStatus == "" || newStatus == r.Entry.Status {
		return
	}
	if err := vault.UpdateStatus(r.Entry.Path, newStatus); err != nil {
		return
	}
	if st != nil {
		st.LogEvent("status-fix", fmt.Sprintf("%q: Status %s → %s", r.Entry.Title, r.Entry.Status, newStatus))
	}
	r.Entry.Status = newStatus
}

func entryReadVolumes(e vault.Entry) *int {
	if !e.HasReadVolumes {
		return nil
	}
	v := e.ReadVolumes
	return &v
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
