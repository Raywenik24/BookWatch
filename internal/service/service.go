// Package service is the orchestration shared by the CLI, the HTTP API, and
// the scheduler: scan → check → (optionally) write → (optionally) record.
package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"bookwatch/internal/checker"
	"bookwatch/internal/provider"
	"bookwatch/internal/scraper"
	"bookwatch/internal/sources"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

// bundleRE matches box-sets, omnibuses, and special-edition reprints — noise
// that survives the date floor because OL indexes the bundle as its own work.
var bundleRE = regexp.MustCompile(`(?i)\b(bundle|box(ed)?\s*set|collection|omnibus|\d+[- ]book|(lettered|limited|rare)\s+edition)\b`)

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
	Checked         int          `json:"checked"`
	Updated         int          `json:"updated"`
	Errors          int          `json:"errors"`
	Suspicious      int          `json:"suspicious"` // scraped OK but looked wrong (broken rules?)
	TrackersChecked int          `json:"trackers_checked"`
	NewReleases     int          `json:"new_releases"`
	TrackingErrors  int          `json:"tracking_errors"` // counted apart from Errors: OL failures, not scrape failures
	Updates         []UpdateInfo `json:"updates"`
}

// RunCheck scans scanRoots (Light Novel + Book roots, deduped when one is
// nested in another), checks each book, optionally writes vault updates,
// polls watched authors for new releases, and (when st != nil) records the
// run + updates + books. ol may be nil to skip the author-tracker phase
// (e.g. in tests that don't care about it). lc may be nil to skip the Polish
// (Lubimyczytać) release pass. progress may be nil.
func RunCheck(sc *scraper.Client, st *store.Store, ol provider.Provider, lc provider.PolishSource, scanRoots []string, write bool,
	progress func(i, total int, title string)) (CheckSummary, error) {

	entries, err := vault.ScanRoots(scanRoots)
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

	if st != nil && ol != nil {
		sum.TrackersChecked, sum.NewReleases, sum.TrackingErrors = pollTrackers(ol, lc, st, progress)
	}

	if st != nil {
		summary := fmt.Sprintf("%d notes, %d updates, %d errors, %d suspicious", sum.Checked, sum.Updated, sum.Errors, sum.Suspicious)
		if sum.TrackersChecked > 0 || sum.TrackingErrors > 0 {
			summary += fmt.Sprintf(" · %d authors polled, %d new releases, %d tracking errors",
				sum.TrackersChecked, sum.NewReleases, sum.TrackingErrors)
		}
		if e := st.FinishRun(runID, sum.Checked, sum.Updated, sum.Errors, summary); e != nil {
			return sum, e
		}
	}
	return sum, nil
}

// pollTrackers polls AuthorWorks for every author tracker and normalizes the
// results into `releases` rows: date-floored against the baseline, narrowed
// to the catalog language, stripped of box-sets/bundles, and deduped against
// every work already seen or surfaced. Same-year-as-baseline ties are kept —
// a dismiss click beats a silent miss. OL failures count separately from scan
// errors so one provider outage doesn't read as a broken LN check. progress
// (when non-nil) reports a second phase — restarting from 0 — so a long
// tracker poll doesn't leave the UI's progress bar stuck at the LN total.
func pollTrackers(ol provider.Provider, lc provider.PolishSource, st *store.Store, progress func(i, total int, title string)) (checked, newReleases, errs int) {
	trackers, err := st.ListTrackers()
	if err != nil {
		return 0, 0, 1
	}

	authorTotal := 0
	for _, t := range trackers {
		if t.Kind == "author" {
			authorTotal++
		}
	}

	for _, t := range trackers {
		if t.Kind != "author" {
			continue
		}
		checked++
		if progress != nil {
			progress(checked, authorTotal, t.Name)
		}

		works, err := ol.AuthorWorks(t.OLKey)
		if err != nil {
			errs++
			continue
		}

		seenIDs, err := st.SeenWorkIDs(t.ID)
		if err != nil {
			errs++
			continue
		}
		surfacedIDs, err := st.ReleaseWorkIDs(t.ID)
		if err != nil {
			errs++
			continue
		}
		seen := make(map[string]bool, len(seenIDs)+len(surfacedIDs)+1)
		for _, id := range seenIDs {
			seen[id] = true
		}
		for _, id := range surfacedIDs {
			seen[id] = true
		}
		seen[t.BaselineWorkID] = true

		baselineYear, _ := strconv.Atoi(t.BaselineDate)

		for _, w := range works {
			if seen[w.WorkID] {
				continue
			}
			if baselineYear > 0 && w.FirstPubYear > 0 && w.FirstPubYear < baselineYear {
				continue // strictly before the baseline — backlist/old translations
			}
			if bundleRE.MatchString(w.Title) {
				continue
			}

			detail, err := ol.WorkDetail(w.WorkID)
			if err != nil {
				continue // best-effort: a single bad work shouldn't fail the whole poll
			}
			if !hasCatalogEdition(detail, t.CatalogLanguage) {
				continue // standalone foreign-translation work, no catalog-language edition
			}

			firstPub := ""
			if w.FirstPubYear > 0 {
				firstPub = strconv.Itoa(w.FirstPubYear)
			}
			cover := provider.SelectCover(detail, t.CatalogLanguage)
			if _, err := st.UpsertRelease(t.ID, w.WorkID, w.Title, t.Name, firstPub, cover, ""); err == nil {
				newReleases++
			}
		}

		// Polish pass: OL fragments Polish editions as language:null works it can't
		// attribute to the author, so they never surface above. Lubimyczytać lists
		// them cleanly (#43). Only for pol-catalog trackers, and best-effort.
		if lc != nil && t.CatalogLanguage == "pol" {
			nr, failed := pollLCReleases(lc, st, t, seen, baselineYear)
			newReleases += nr
			if failed {
				errs++
			}
		}

		// Polish-translation pass (#46): opt-in, only for a non-Polish-catalog
		// tracker. Re-checks every primary-language release ever surfaced (not
		// just this run's) for a Polish edition, since a translation can lag the
		// original by a year or more.
		if lc != nil && t.CatalogLanguage != "pol" && t.WatchPolishTranslation {
			nr, failed := pollPolishTranslations(lc, st, t, seen)
			newReleases += nr
			if failed {
				errs++
			}
		}
	}
	return checked, newReleases, errs
}

// pollLCReleases surfaces an author's Polish releases from Lubimyczytać — the
// editions OL can't attribute to the author. It reuses the poll's dedup set and
// baseline floor, and marks each surfaced work seen so a duplicate id in the
// bibliography can't surface twice. LC work ids are namespaced ("cykl:"/"lc:")
// so they never collide with the OL work ids already in `seen`. Any failure
// returns a failed flag without aborting the wider poll; an author simply absent
// from Lubimyczytać is not a failure.
func pollLCReleases(lc provider.PolishSource, st *store.Store, t store.Tracker, seen map[string]bool, baselineYear int) (newReleases int, failed bool) {
	path := lc.AuthorSearch(t.Name)
	if path == "" {
		return 0, false
	}
	works, err := lc.AuthorWorks(path)
	if err != nil {
		return 0, true
	}
	for _, w := range works {
		if w.WorkID == "" || seen[w.WorkID] {
			continue
		}
		if baselineYear > 0 && w.FirstPubYear > 0 && w.FirstPubYear < baselineYear {
			continue
		}
		if bundleRE.MatchString(w.Title) {
			continue
		}
		seen[w.WorkID] = true
		firstPub := ""
		if w.FirstPubYear > 0 {
			firstPub = strconv.Itoa(w.FirstPubYear)
		}
		if _, err := st.UpsertRelease(t.ID, w.WorkID, w.Title, t.Name, firstPub, w.CoverURL, ""); err == nil {
			newReleases++
		}
	}
	return newReleases, false
}

// plTranslationPrefix namespaces a translation-of release's work id off its
// primary release's OL work id (rather than the raw Lubimyczytać hit id), so
// (a) it can never collide with a real OL or "cykl:"/"lc:" id and (b) its
// presence in `seen` (via ReleaseWorkIDs, loaded once per tracker at the top
// of pollTrackers) tells the next poll a translation was already found —
// without a second network round-trip.
const plTranslationPrefix = "pl-tr:"

// pollPolishTranslations is the opt-in translation-watch pass (#46): for every
// primary-language release this tracker has ever surfaced, ask Lubimyczytać
// (by the release's own title+author — not a full bibliography scan) whether a
// Polish edition now exists. A release that already has one (per `seen`) is
// skipped. A hit is surfaced as its own release, kind="translation-of", so the
// UI can tell it apart. Best-effort like the other Polish passes: a lookup
// failure never aborts the tracker, and there is no "failed" signal here since
// MatchWork never errors (a miss is just !Found).
func pollPolishTranslations(lc provider.PolishSource, st *store.Store, t store.Tracker, seen map[string]bool) (newReleases int, failed bool) {
	primaries, err := st.PrimaryReleases(t.ID)
	if err != nil {
		return 0, true
	}
	for _, r := range primaries {
		key := plTranslationPrefix + r.WorkID
		if seen[key] {
			continue
		}
		m := lc.MatchWork(r.Title, r.Author, nil)
		if !m.Found {
			continue
		}
		seen[key] = true
		if _, err := st.UpsertRelease(t.ID, key, m.Title, t.Name, r.FirstPubDate, m.CoverURL, "translation-of"); err == nil {
			newReleases++
		}
	}
	return newReleases, false
}

// hasCatalogEdition reports whether w has an edition in lang. An unset lang
// imposes no constraint.
func hasCatalogEdition(w provider.Work, lang string) bool {
	if lang == "" {
		return true
	}
	for _, e := range w.Editions {
		if e.Language == lang {
			return true
		}
	}
	return false
}

// determineStatusCorrection returns the corrected status for an entry given
// the check result, or "" if no correction is needed. Dropped is never touched.
func determineStatusCorrection(e vault.Entry, hasNew bool) string {
	if strings.EqualFold(e.Status, "Dropped") {
		return ""
	}
	readVols := e.ReadVolumes // 0 when !HasReadVolumes
	if hasNew && strings.EqualFold(e.Status, "Completed") {
		return "Backlog"
	}
	if readVols < e.Volumes && strings.EqualFold(e.Status, "Completed") {
		return "Backlog"
	}
	if !hasNew && e.Volumes > 0 && readVols == e.Volumes && strings.EqualFold(e.Status, "Backlog") {
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

// ApplyPending writes the selected pending updates' stored volume counts to
// their vault notes (vault.UpdateVolumes), bumps each book, and stamps each
// update applied. It applies the LAST check's numbers — it does not re-scrape.
// Only updates whose ID is in ids are touched (writing to the vault is always
// a deliberate, ticked choice — see issue #36).
func ApplyPending(st *store.Store, today string, ids []int64) (ApplyResult, error) {
	pending, err := st.ListPending()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("list pending: %w", err)
	}
	selected := make(map[int64]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	var res ApplyResult
	for _, p := range pending {
		if !selected[p.ID] {
			continue
		}
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
