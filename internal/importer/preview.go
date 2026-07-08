package importer

import "bookwatch/internal/calibre"

// Preview is the dry-run scope report (#75): a cheap, network-free pass over the
// Calibre library that reports what an import would cover, so a wrong library
// path (or an unexpectedly huge scope) is caught before anything is matched or
// written. Counts are over *new* work — books already handled by a prior session
// are excluded — so a re-run's preview shows only what's left to do.
type Preview struct {
	LNSeries     int `json:"ln_series"`     // distinct light-novel series (incl. standalones)
	LNVolumes    int `json:"ln_volumes"`    // owned volumes across those series
	RegularBooks int `json:"regular_books"` // non-LN books
	Duplicates   int `json:"duplicates"`    // items whose title already matches a tracked note
	AlreadyDone  int `json:"already_done"`  // work units skipped as already processed
}

// BuildPreview computes the dry-run counts. dup may be nil (skips the duplicate
// estimate); processed is the set of already-handled Calibre uuids.
func BuildPreview(books []calibre.Book, dup *DupIndex, processed map[string]bool) Preview {
	var p Preview

	// Count already-processed units against the full (unfiltered) grouping so the
	// preview can report how much a re-run skips.
	all := GroupWorkUnits(books, nil)
	remaining := GroupWorkUnits(books, processed)
	p.AlreadyDone = len(all) - len(remaining)

	for _, u := range remaining {
		switch u.kind {
		case KindLNSeries:
			p.LNSeries++
			p.LNVolumes += len(u.books)
		default:
			p.RegularBooks++
		}
		if dup != nil {
			// Title-only estimate: matching hasn't run yet, so there's no resolved
			// link to check — a cheap heads-up, not the authoritative dup flag the
			// staged notes carry.
			if _, ok := dup.Lookup("", u.title); ok {
				p.Duplicates++
			}
		}
	}
	return p
}
