package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bookwatch/internal/notes"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

// reportName is the import report note, written inside the staging dir (so it
// travels with the review notes and stays out of the scan roots).
const reportName = "_CalibreImport-report.md"

// ReportSummary is the tally a report section records.
type ReportSummary struct {
	Total      int
	Matched    int
	Unmatched  int
	Duplicates int
	Errored    int
}

// WriteReport appends a dated section to the staging dir's import report,
// summarizing the session's items (matched/unmatched/errored, notes staged) and
// listing the ones that need a human — unmatched notes to resolve, possible
// duplicates to review, and errored units to retry. The unmatched + duplicate
// lists are Obsidian `[[wikilinks]]` to the staged notes, so the report is a
// clickable review checklist. Returns the report path. Appends rather than
// overwrites so a resumed or repeated import keeps its history.
func WriteReport(stagingDir, today string, items []store.ImportItem) (string, ReportSummary, error) {
	var sum ReportSummary
	var unmatched, duplicates, errored []store.ImportItem
	for _, it := range items {
		sum.Total++
		switch it.State {
		case "matched":
			sum.Matched++
		case "unmatched":
			sum.Unmatched++
			unmatched = append(unmatched, it)
		case "errored":
			sum.Errored++
			errored = append(errored, it)
		}
		// A duplicate is orthogonal to match state (a staged note can be both
		// unmatched and a duplicate) — count/list it whenever the app flagged one.
		if it.DuplicateOf != "" {
			sum.Duplicates++
			duplicates = append(duplicates, it)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Import %s\n\n", today)
	fmt.Fprintf(&b, "- **%d** work units: %d matched, %d unmatched, %d errored.\n",
		sum.Total, sum.Matched, sum.Unmatched, sum.Errored)
	fmt.Fprintf(&b, "- **%d** notes staged for review", sum.Matched+sum.Unmatched)
	if sum.Duplicates > 0 {
		fmt.Fprintf(&b, " (%d flagged as possible duplicates)", sum.Duplicates)
	}
	b.WriteString(".\n\n")
	if len(unmatched) > 0 {
		b.WriteString("### Unmatched — resolve by hand\n\n")
		b.WriteString("Open each note, pick the right candidate link in its body (or fix `Link:` yourself), then remove the `#import/unmatched` tag.\n\n")
		for _, it := range unmatched {
			fmt.Fprintf(&b, "- [[%s]]\n", notes.Sanitize(it.Title, false))
		}
		b.WriteString("\n")
	}
	if len(duplicates) > 0 {
		b.WriteString("### Possible duplicates — review before finalizing\n\n")
		b.WriteString("Each is a *proposal* tagged `#import/duplicate`; hand-merge into the existing note and delete the proposal, or keep it. Finalize skips any whose name already exists, so it can't overwrite the original.\n\n")
		for _, it := range duplicates {
			fmt.Fprintf(&b, "- [[%s]] — duplicate of [[%s]]\n", notes.Sanitize(it.Title, false), it.DuplicateOf)
		}
		b.WriteString("\n")
	}
	if len(errored) > 0 {
		b.WriteString("### Errored — retry\n\n")
		for _, it := range errored {
			fmt.Fprintf(&b, "- %s — %s\n", it.Title, it.Error)
		}
		b.WriteString("\n")
	}

	path := filepath.Join(stagingDir, reportName)
	if err := appendReport(path, b.String()); err != nil {
		return "", sum, err
	}
	return path, sum, nil
}

// WriteFinalizeReport appends a dated "Finalize" section to the staging dir's
// import report: how many notes + covers were moved into the vault, any notes
// skipped because their target already existed, and the Excluded-files hint the
// user should paste into Obsidian's Settings → Files & links so the moved
// `_volumes` archive isn't indexed. excludeHint is vault-relative.
func WriteFinalizeReport(stagingDir, today string, res FinalizeResult, excludeHint string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## Finalize %s\n\n", today)
	fmt.Fprintf(&b, "- Moved **%d** notes and **%d** covers into the vault.\n", res.Notes, res.Covers)
	if len(res.Skipped) > 0 {
		fmt.Fprintf(&b, "- Skipped **%d** (a note with that name already exists — review by hand):\n", len(res.Skipped))
		for _, s := range res.Skipped {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
	}
	b.WriteString("\n> [!info] Exclude the volume archive from Obsidian\n")
	fmt.Fprintf(&b, "> Add `%s` to **Settings → Files & links → Excluded files** so the untracked volume notes stay out of search and the graph. (The app never touches `.obsidian`.)\n", excludeHint)

	path := filepath.Join(stagingDir, reportName)
	if err := appendReport(path, b.String()); err != nil {
		return "", err
	}
	return path, nil
}

// appendReport appends a section to the report (long-path-safe), creating it —
// with a title header — on first write.
func appendReport(path, section string) error {
	full := longPath(path)
	prior, err := os.ReadFile(full)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out string
	if len(prior) == 0 {
		out = "# Calibre Import Report\n\n" + section
	} else {
		out = string(prior) + "\n" + section
	}
	if err := os.MkdirAll(longPath(filepath.Dir(path)), 0o755); err != nil {
		return err
	}
	return vault.AtomicWrite(full, []byte(out), 0o644)
}
