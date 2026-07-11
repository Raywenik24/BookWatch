// Orchestration for the Calibre import (milestone 1.2.0, #75). Part 5 of 7.
//
// This drives the whole import: read the Calibre library (done by the caller,
// #71), group light-novel volumes into series, match each work unit (#73), and
// stage its notes (#74) — one item at a time, writing each staged note to disk
// immediately so a long run that's stopped or crashes always leaves partial
// work behind.
//
// A run is backed by a resumable SQLite session (store, #75): every work unit
// is one import_items row keyed by its primary Calibre uuid. Matched/unmatched
// items are recorded processed so a Resume re-reads the library and simply
// skips what's done, continuing from the first unprocessed unit; errored items
// (a network/status failure, distinct from a clean "no match") stay unprocessed
// and are retried. Start-over discards the session and the app-written files;
// idempotency (import_processed) means a later re-run only picks up newly-added
// Calibre books.
//
// The package stays decoupled from HTTP and the live scraper: the jnovels
// volume-count scrape is injected as ScrapeSeries, so the whole driver is
// exercised against a temp vault + temp DB in tests.
package importer

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"

	"bookwatch/internal/calibre"
	"bookwatch/internal/store"
)

// processedStates are the item states that count as "handled" — the ones whose
// Calibre uuids are recorded in import_processed and that a Resume skips. An
// errored item is deliberately excluded so it's retried.
var processedStates = map[string]bool{"matched": true, "unmatched": true}

// Import drives one session's worth of work. Build it with the matched backends
// + a staging Writer + a DupIndex snapshot of the vault, then call Run (for a
// start or resume). ScrapeSeries and Progress may be nil.
type Import struct {
	Store   *store.Store
	Matcher *Matcher
	Writer  *Writer
	Dup     *DupIndex
	Today   string

	// ScrapeSeries fetches a matched jnovels series link's volume count and
	// description fallback. Injected by the caller (which owns the scraper +
	// source rules); nil skips the scrape, staging the Calibre volume count.
	ScrapeSeries func(link string) (volumes int, description string)

	// ScrapeBookDescription fetches a matched regular book's blurb from its
	// resolved source (OpenLibrary / Lubimyczytać), used only when Calibre carries
	// no description of its own. Injected by the caller (which owns the providers);
	// nil, an unmatched item, or a source with no blurb leaves the description
	// blank. kind picks the source.
	ScrapeBookDescription func(kind Kind, link, workID string) string

	// Progress, if set, is called before each unit is processed with the number
	// already done, the session total, and the unit's title — mirroring the
	// check run so the UI's progress bar is driven the same way.
	Progress func(done, total int, title string)
}

// workUnit is one thing to match+stage: an LN series with its owned volumes, or
// a single regular book. key is the primary Calibre uuid (idempotency key).
type workUnit struct {
	seq    int
	kind   Kind
	key    string
	series string // LN series name, or a regular book's series ("" if standalone)
	title  string
	books  []calibre.Book // LN volumes, or the single regular book
}

// GroupWorkUnits reads the library into deterministic work units, dropping any
// unit whose every member Calibre uuid is already processed (so a re-run only
// surfaces new books). Order is stable (first appearance), which fixes each
// unit's seq for the session.
func GroupWorkUnits(books []calibre.Book, processed map[string]bool) []workUnit {
	var lnOrder []string
	lnGroups := map[string][]calibre.Book{}
	var units []workUnit

	for _, b := range books {
		switch Classify(b) {
		case KindLNVolume: // owned volume of a series → grouped under the series
			key := b.Series
			if _, ok := lnGroups[key]; !ok {
				lnOrder = append(lnOrder, key)
			}
			lnGroups[key] = append(lnGroups[key], b)
		case KindLNSeries: // standalone light novel → its own single-volume series
			units = append(units, workUnit{kind: KindLNSeries, series: b.Title, title: b.Title, books: []calibre.Book{b}})
		default: // regular book (English / Polish)
			units = append(units, workUnit{kind: Classify(b), series: b.Series, title: b.Title, books: []calibre.Book{b}})
		}
	}
	// Fold the grouped LN series in first-appearance order.
	for _, name := range lnOrder {
		vols := lnGroups[name]
		sort.SliceStable(vols, func(i, j int) bool { return vols[i].SeriesIndex < vols[j].SeriesIndex })
		units = append(units, workUnit{kind: KindLNSeries, series: name, title: name, books: vols})
	}

	// Assign keys + seq, and filter fully-processed units.
	out := make([]workUnit, 0, len(units))
	seq := 0
	for _, u := range units {
		u.key = bookKey(u.books[0])
		if allProcessed(u.books, processed) {
			continue
		}
		u.seq = seq
		seq++
		out = append(out, u)
	}
	return out
}

// Run processes every not-yet-done unit of the session, staging as it goes and
// recording each item's state. It halts early (leaving the session resumable)
// when a stop is requested; when it drains the queue it marks the session done.
// units come from GroupWorkUnits (already filtered + sequenced).
func (im *Import) Run(sessionID int64, units []workUnit) error {
	// Fix the total on the first pass only, so a Resume (which re-enumerates just
	// the remaining units) keeps the original "240/511" denominator.
	if sess, ok, err := im.Store.GetImportSession(sessionID); err != nil {
		return err
	} else if ok && sess.Total == 0 {
		if err := im.Store.SetImportTotal(sessionID, len(units)); err != nil {
			return err
		}
	}
	// Series-index anchors: the first regular book of a multi-book regular series
	// also stages the inert index note (recorded on that book's item).
	anchors, members := regularSeriesIndex(units)

	for i, u := range units {
		stop, err := im.Store.ImportStopRequested(sessionID)
		if err != nil {
			return err
		}
		if stop {
			return nil // halt after the previous item; session stays 'running'
		}
		if im.Progress != nil {
			im.Progress(i, len(units), u.title)
		}
		if err := im.processUnit(sessionID, u, anchors, members); err != nil {
			return err
		}
	}
	return im.Store.FinishImportSession(sessionID, "done")
}

// processUnit matches, stages, and records one work unit.
func (im *Import) processUnit(sessionID int64, u workUnit, anchors map[string]string, members map[string][]string) error {
	it := store.ImportItem{
		SessionID: sessionID,
		Seq:       u.seq,
		Kind:      u.kind.String(),
		Title:     u.title,
		UUID:      u.key,
		UUIDs:     jsonArray(memberKeys(u.books)),
	}

	var res Result
	if u.kind == KindLNSeries {
		res = im.Matcher.MatchSeries(u.series, joinAuthors(u.books[0].Authors))
	} else {
		res = im.Matcher.Match(u.books[0])
	}

	if res.Err != nil {
		it.State = "errored"
		it.Error = res.Err.Error()
		return im.Store.UpsertImportItem(it)
	}

	var staged StageResult
	var err error
	if u.kind == KindLNSeries {
		staged, err = im.stageLN(u, res)
	} else {
		staged, err = im.stageBook(u, res, anchors, members)
	}
	if err != nil {
		it.State = "errored"
		it.Error = err.Error()
		return im.Store.UpsertImportItem(it)
	}

	it.ResolvedLink = res.ResolvedLink
	it.Candidates = jsonCandidates(res.Candidates)
	it.StagedFiles = jsonArray(staged.all())
	it.DuplicateOf = staged.DuplicateOf
	it.State = "matched"
	if res.Unmatched {
		it.State = "unmatched"
	}
	if err := im.Store.UpsertImportItem(it); err != nil {
		return err
	}
	if processedStates[it.State] {
		return im.Store.MarkProcessedUUIDs(memberKeys(u.books))
	}
	return nil
}

// stageLN builds and writes an LN series note (+ volume notes). A matched link
// is scraped for its jnovels volume count + description fallback.
func (im *Import) stageLN(u workUnit, res Result) (StageResult, error) {
	first := u.books[0]
	p := PlanLNSeries{
		Series:      u.series,
		Author:      joinAuthors(first.Authors),
		Language:    firstLang(first),
		Link:        res.ResolvedLink,
		Unmatched:   res.Unmatched,
		Candidates:  res.Candidates,
		Description: first.Comments,
		CoverPath:   first.CoverPath,
	}
	if !res.Unmatched && im.ScrapeSeries != nil {
		p.JnovelsVolumes, p.JnovelsDescription = im.ScrapeSeries(res.ResolvedLink)
	}
	if im.Dup != nil {
		if existing, ok := im.Dup.Lookup(res.ResolvedLink, u.series); ok {
			p.ExistingNote = existing
		}
	}
	for _, b := range u.books {
		p.Volumes = append(p.Volumes, PlanVolume{
			Title:       b.Title,
			SeriesIndex: b.SeriesIndex,
			Language:    firstLang(b),
			Released:    pubYear(b.PubDate),
			Description: b.Comments,
			CoverPath:   b.CoverPath,
			Done:        hasTag(b.Tags, "done"),
		})
	}
	return im.Writer.StageLNSeries(p)
}

// stageBook builds and writes a regular #Book note, plus the series-index note
// when this book anchors a multi-book regular series.
func (im *Import) stageBook(u workUnit, res Result, anchors map[string]string, members map[string][]string) (StageResult, error) {
	b := u.books[0]
	// Calibre's own comments win; fall back to the matched source's blurb when it
	// has none (many Polish/older imports carry no comments even though the
	// catalog page does).
	desc := b.Comments
	if strings.TrimSpace(desc) == "" && !res.Unmatched && im.ScrapeBookDescription != nil {
		desc = im.ScrapeBookDescription(u.kind, res.ResolvedLink, res.WorkID)
	}
	p := PlanBook{
		Title:       b.Title,
		Author:      joinAuthors(b.Authors),
		Language:    firstLang(b),
		Series:      b.Series,
		SeriesIndex: b.SeriesIndex,
		Link:        res.ResolvedLink,
		WorkID:      res.WorkID,
		Unmatched:   res.Unmatched,
		Candidates:  res.Candidates,
		Done:        hasTag(b.Tags, "done"),
		ReleasedEN:  pubYear(b.PubDate),
		Description: desc,
		CoverPath:   b.CoverPath,
	}
	if im.Dup != nil {
		if existing, ok := im.Dup.Lookup(res.ResolvedLink, b.Title); ok {
			p.ExistingNote = existing
		}
	}
	staged, err := im.Writer.StageBook(p)
	if err != nil {
		return staged, err
	}
	// Anchor book of a multi-book regular series → also stage the inert index.
	if b.Series != "" && anchors[b.Series] == u.key {
		idx, ierr := im.Writer.StageSeriesIndex(PlanSeriesIndex{
			Series:   b.Series,
			Language: firstLang(b),
			Volumes:  members[b.Series],
		})
		if ierr != nil {
			return staged, ierr
		}
		staged.VolumeNotes = append(staged.VolumeNotes, idx.Note)
	}
	return staged, nil
}

// StartOver discards a session: removes every app-written staged file, forgets
// its processed uuids (so a re-run sees those books as new), and deletes the
// session row. User edits are safe — only files the app recorded are removed.
func StartOver(st *store.Store, sessionID int64) error {
	items, err := st.ListImportItems(sessionID)
	if err != nil {
		return err
	}
	var uuids []string
	for _, it := range items {
		for _, f := range parseJSONArray(it.StagedFiles) {
			os.Remove(longPath(f))
		}
		uuids = append(uuids, parseJSONArray(it.UUIDs)...)
	}
	if err := st.ForgetProcessedUUIDs(uuids); err != nil {
		return err
	}
	return st.DeleteImportSession(sessionID)
}

// RetryFailures re-queues the session's unmatched + errored items: it forgets
// the processed uuids of unmatched units (errored ones were never processed),
// then flips both back to pending so the next Run reprocesses them.
func RetryFailures(st *store.Store, sessionID int64) error {
	items, err := st.ListImportItems(sessionID)
	if err != nil {
		return err
	}
	var uuids []string
	for _, it := range items {
		if it.State == "unmatched" {
			uuids = append(uuids, parseJSONArray(it.UUIDs)...)
		}
	}
	if err := st.ForgetProcessedUUIDs(uuids); err != nil {
		return err
	}
	if err := st.ResetImportItems(sessionID, []string{"unmatched", "errored"}); err != nil {
		return err
	}
	return st.FinishImportSession(sessionID, "running")
}

// ── helpers ────────────────────────────────────────────────────

// regularSeriesIndex returns, for each regular (non-LN) series with ≥2 books:
// the anchor unit key that should stage the index, and the ordered member
// titles the index lists.
func regularSeriesIndex(units []workUnit) (anchors map[string]string, members map[string][]string) {
	anchors, members = map[string]string{}, map[string][]string{}
	for _, u := range units {
		if u.kind == KindLNSeries {
			continue
		}
		b := u.books[0]
		if b.Series == "" {
			continue
		}
		if _, ok := anchors[b.Series]; !ok {
			anchors[b.Series] = u.key
		}
		members[b.Series] = append(members[b.Series], b.Title)
	}
	for name, titles := range members {
		if len(titles) < 2 {
			delete(anchors, name) // a lone book in a "series" gets no index note
			delete(members, name)
		}
	}
	return anchors, members
}

// bookKey is a book's idempotency key: its Calibre uuid, or a synthetic id-based
// key for the rare very-old library whose rows have no uuid.
func bookKey(b calibre.Book) string {
	if strings.TrimSpace(b.UUID) != "" {
		return b.UUID
	}
	return "cid:" + strconv.FormatInt(b.ID, 10)
}

func memberKeys(books []calibre.Book) []string {
	out := make([]string, 0, len(books))
	for _, b := range books {
		out = append(out, bookKey(b))
	}
	return out
}

func allProcessed(books []calibre.Book, processed map[string]bool) bool {
	for _, b := range books {
		if !processed[bookKey(b)] {
			return false
		}
	}
	return len(books) > 0
}

func joinAuthors(authors []string) string { return strings.Join(authors, ", ") }

func firstLang(b calibre.Book) string {
	if len(b.Languages) > 0 {
		return b.Languages[0]
	}
	return ""
}

// pubYear pulls a plausible 4-digit year out of a Calibre pubdate, dropping the
// junk sentinel dates (year 0101, etc.) Calibre stores for unknown dates.
func pubYear(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) < 4 {
		return ""
	}
	y, err := strconv.Atoi(raw[:4])
	if err != nil || y < 1400 || y > 2200 {
		return ""
	}
	return raw[:4]
}

func jsonArray(v []string) string {
	if len(v) == 0 {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonCandidates(c []Candidate) string {
	if len(c) == 0 {
		return ""
	}
	b, _ := json.Marshal(c)
	return string(b)
}

func parseJSONArray(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// all flattens a StageResult's written paths (note + volume notes + covers) for
// the item's staged-files record, used by Start-over cleanup.
func (r StageResult) all() []string {
	out := make([]string, 0, 1+len(r.VolumeNotes)+len(r.Covers))
	if r.Note != "" {
		out = append(out, r.Note)
	}
	out = append(out, r.VolumeNotes...)
	out = append(out, r.Covers...)
	return out
}
