package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

// reportName is the import report note, written inside the staging dir (so it
// travels with the review notes and stays out of the scan roots).
const reportName = "_CalibreImport-report.md"

// ReportSummary is the tally a report section records.
type ReportSummary struct {
	Total     int
	Matched   int
	Unmatched int
	Errored   int
}

// WriteReport appends a dated section to the staging dir's import report,
// summarizing the session's items (matched/unmatched/errored, notes staged) and
// listing the ones that need a human — unmatched notes to resolve and errored
// units to retry. Returns the report path. Appends rather than overwrites so a
// resumed or repeated import keeps its history.
func WriteReport(stagingDir, today string, items []store.ImportItem) (string, ReportSummary, error) {
	var sum ReportSummary
	var unmatched, errored []store.ImportItem
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
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Import %s\n\n", today)
	fmt.Fprintf(&b, "- **%d** work units: %d matched, %d unmatched, %d errored.\n",
		sum.Total, sum.Matched, sum.Unmatched, sum.Errored)
	fmt.Fprintf(&b, "- **%d** notes staged for review.\n\n", sum.Matched+sum.Unmatched)
	if len(unmatched) > 0 {
		b.WriteString("### Unmatched — resolve by hand\n\n")
		for _, it := range unmatched {
			fmt.Fprintf(&b, "- %s\n", it.Title)
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
