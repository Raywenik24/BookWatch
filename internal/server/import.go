package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"bookwatch/internal/calibre"
	"bookwatch/internal/importer"
	"bookwatch/internal/provider"
	"bookwatch/internal/service"
	"bookwatch/internal/sources"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

// importMinGap is the per-request politeness floor the import matcher applies to
// every OpenLibrary / jnovels lookup, so a 500-book library can't hammer a
// source into a block. Lubimyczytać self-throttles on top of it.
const importMinGap = 400 // milliseconds

// ── status ─────────────────────────────────────────────────────

// handleImportStatus reports the current session's progress — the "240/511 —
// Resume" readout. Open (read-only) like /api/status. When no session exists it
// reports state:"idle" so the UI shows the plain Start button.
func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request) {
	s.reconcileImportSession()
	writeJSON(w, http.StatusOK, s.importStatusPayload())
}

// reconcileImportSession resets all import state when the staging folder has been
// abandoned — the folder is the source of truth, so if the user deletes it (or
// empties it of notes) the import is over. Without this the durable
// import_processed idempotency outlives the on-disk notes and a fresh preview
// keeps reporting the prior run's books as "already done", even across the
// several stale sessions a few re-runs leave behind.
//
// It wipes *everything* (all sessions + the whole processed set — `ResetImport`)
// rather than one session, since "already done" is global and re-runs pile up
// multiple finished sessions. Gates:
//   - skipped while a run is in flight (busy) or a session is active/stopped
//     (ActiveImportSession) — those are handled by Resume / Stop / Start-over, and
//     the gate also avoids a race with a just-created run session;
//   - only fires when there's actually state to clear and no staged notes remain
//     on disk. A completed Finalize (notes moved to the vault) also leaves no
//     notes — fine: the kept books are tracked, and a later re-run re-proposing
//     them is dup-detection + collision-safe.
func (s *Server) reconcileImportSession() {
	s.importMu.Lock()
	busy := s.importBusy
	s.importMu.Unlock()
	if busy {
		return
	}
	if _, active, err := s.st.ActiveImportSession(); err != nil || active {
		return // a run in progress or a stopped/resumable session — leave it be
	}
	_, hasSession, _ := s.st.LatestImportSession()
	proc, _ := s.st.ProcessedUUIDs()
	if !hasSession && len(proc) == 0 {
		return // nothing to reconcile
	}
	if s.stagingHasNotes() {
		return // notes still staged for review/finalize — not abandoned
	}
	if err := s.st.ResetImport(); err == nil {
		s.st.LogEvent("import", "Staging empty — Calibre import state reset")
		s.publishImportStatus()
	}
}

// stagingHasNotes reports whether the staging folder still holds any staged note
// (a `.md` other than the import report). A missing folder counts as no notes.
func (s *Server) stagingHasNotes() bool {
	dir := s.effectiveImportStagingDir()
	found := false
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(strings.ToLower(name), ".md") && name != "_CalibreImport-report.md" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// importStatusPayload is the shared import-status shape served by the status
// endpoint and pushed over the SSE stream. done/total come from the persisted
// item states so the figure is correct even after a restart; cur/title track the
// in-flight unit while a run is live.
func (s *Server) importStatusPayload() map[string]any {
	s.importMu.Lock()
	busy, title := s.importBusy, s.importTitle
	s.importMu.Unlock()

	// The review/finalize flow works off the *latest* session (a fully-completed
	// run leaves a 'done' session ActiveImportSession wouldn't return), so a
	// finished import still surfaces its summary + review controls.
	sess, ok, err := s.st.LatestImportSession()
	if err != nil || !ok {
		return map[string]any{"state": "idle", "busy": busy}
	}
	items, _ := s.st.ListImportItems(sess.ID)
	done, unmatched, duplicates, errored := 0, 0, 0, 0
	for _, it := range items {
		switch it.State {
		case "matched":
			done++
		case "unmatched":
			done++
			unmatched++
		case "errored":
			errored++
		}
		if it.DuplicateOf != "" {
			duplicates++
		}
	}
	var state string
	switch {
	case busy:
		state = "running"
	case sess.Status != "done":
		state = "resumable" // stopped mid-run — resumable
	case s.reviewableCount(items) > 0:
		state = "review" // finished; notes still staged for review/finalize
	default:
		return map[string]any{"state": "idle", "busy": busy} // finished + nothing left staged
	}
	return map[string]any{
		"state":         state,
		"busy":          busy,
		"session_id":    sess.ID,
		"total":         sess.Total,
		"done":          done,
		"unmatched":     unmatched,
		"duplicates":    duplicates,
		"errored":       errored,
		"current_title": title,
		"staging_dir":   sess.StagingDir,
	}
}

// publishImportStatus pushes a fresh import-status frame to SSE subscribers.
func (s *Server) publishImportStatus() { s.stream.Publish("import", s.importStatusPayload()) }

// ── preview (dry-run) ──────────────────────────────────────────

// handleImportPreview runs the cheap, network-free scope pass (#75): read the
// configured library, group + count what a run would cover, so a wrong library
// path is caught before anything is written.
func (s *Server) handleImportPreview(w http.ResponseWriter, r *http.Request) {
	s.reconcileImportSession()
	libPath := s.effectiveCalibreLibraryPath()
	if libPath == "" {
		writeJSON(w, http.StatusBadRequest, errBody("no Calibre library path configured — set it in Settings"))
		return
	}
	books, err := calibre.Read(libPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("read Calibre library: "+err.Error()))
		return
	}
	books = s.importFilter().Apply(books)
	dup := s.buildDupIndex()
	proc, _ := s.st.ProcessedUUIDs()
	writeJSON(w, http.StatusOK, importer.BuildPreview(books, dup, proc))
}

// ── start / resume ─────────────────────────────────────────────

// handleImportStart starts a new import, or resumes the active one. A resume
// clears the stop flag and continues from the first unprocessed unit, reusing
// every already-recorded match. The run itself happens on a background
// goroutine; this returns 202 immediately and progress flows over SSE.
func (s *Server) handleImportStart(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	busy := s.importBusy
	s.importMu.Unlock()
	if busy {
		writeJSON(w, http.StatusConflict, errBody("an import is already running"))
		return
	}
	s.reconcileImportSession() // a deleted staging folder → fresh start, not a resume

	sess, active, err := s.st.ActiveImportSession()
	if err != nil {
		writeErr(w, err)
		return
	}
	var sid int64
	if active {
		sid = sess.ID
		if err := s.st.ClearImportStop(sid); err != nil {
			writeErr(w, err)
			return
		}
	} else {
		libPath := s.effectiveCalibreLibraryPath()
		if libPath == "" {
			writeJSON(w, http.StatusBadRequest, errBody("no Calibre library path configured — set it in Settings"))
			return
		}
		sid, err = s.st.CreateImportSession(libPath, s.effectiveImportStagingDir())
		if err != nil {
			writeJSON(w, http.StatusConflict, errBody(err.Error()))
			return
		}
		s.st.LogEvent("import", "Started Calibre import")
	}
	s.runImport(sid)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "session_id": sid})
}

// handleImportStop flags the running import to halt after the current item. The
// session stays resumable.
func (s *Server) handleImportStop(w http.ResponseWriter, r *http.Request) {
	sess, ok, err := s.st.ActiveImportSession()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no active import"})
		return
	}
	if err := s.st.RequestImportStop(sess.ID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

// handleImportRetry re-queues the active session's unmatched + errored units and
// resumes the run so just those are reprocessed.
func (s *Server) handleImportRetry(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	busy := s.importBusy
	s.importMu.Unlock()
	if busy {
		writeJSON(w, http.StatusConflict, errBody("an import is already running"))
		return
	}
	sess, ok, err := s.st.ActiveImportSession()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("no import session to retry"))
		return
	}
	if err := importer.RetryFailures(s.st, sess.ID); err != nil {
		writeErr(w, err)
		return
	}
	s.st.ClearImportStop(sess.ID)
	s.runImport(sess.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "session_id": sess.ID})
}

// handleImportStartOver discards the active session and every note the app wrote
// for it (user edits are untouched), and forgets its processed uuids so a fresh
// run re-imports from scratch.
func (s *Server) handleImportStartOver(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	busy := s.importBusy
	s.importMu.Unlock()
	if busy {
		writeJSON(w, http.StatusConflict, errBody("stop the running import before starting over"))
		return
	}
	// Latest (not just active) so a completed 'done' session can also be discarded
	// — wiping its staged notes + processed uuids for a clean re-import.
	sess, ok, err := s.st.LatestImportSession()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"status": "nothing to discard"})
		return
	}
	if err := importer.StartOver(s.st, sess.ID); err != nil {
		writeErr(w, err)
		return
	}
	s.st.LogEvent("import", "Discarded Calibre import session")
	s.publishImportStatus()
	writeJSON(w, http.StatusOK, map[string]string{"status": "discarded"})
}

// ── finalize ───────────────────────────────────────────────────

// handleImportFinalize moves the notes still in the staging folder — the ones
// the reviewer kept after checking them in Obsidian — into their real
// destinations, then refreshes the vault so they get tracked. Non-destructive:
// existing notes are never overwritten (a collision is skipped and reported),
// and only the cover a surviving note references travels with it. Runs
// synchronously (a local file move, no network) and returns the outcome.
func (s *Server) handleImportFinalize(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	busy := s.importBusy
	s.importMu.Unlock()
	if busy {
		writeJSON(w, http.StatusConflict, errBody("stop the running import before finalizing"))
		return
	}

	dest, lnNoteRel := s.finalizeDest()
	stagingDir := s.effectiveImportStagingDir()
	res, err := importer.Finalize(stagingDir, dest)
	if err != nil {
		writeErr(w, err)
		return
	}

	// Track the moved notes (existing offline reconcile), then record the outcome
	// + the Excluded-files hint in the report.
	refresh, _ := service.RefreshVault(s.st, ScanRoots(s.cfg, s.st))
	excludeHint := filepath.ToSlash(filepath.Join(lnNoteRel, "_volumes"))
	importer.WriteFinalizeReport(stagingDir, vault.Today(), res, excludeHint)
	s.st.LogEvent("import", fmt.Sprintf("Finalized Calibre import: %d notes, %d covers moved, %d skipped",
		res.Notes, res.Covers, len(res.Skipped)))
	s.publishImportStatus()

	writeJSON(w, http.StatusOK, map[string]any{
		"notes":        res.Notes,
		"covers":       res.Covers,
		"skipped":      res.Skipped,
		"exclude_hint": excludeHint,
		"refresh":      refresh,
	})
}

// ── driver ─────────────────────────────────────────────────────

// runImport launches the session's processing on a background goroutine under
// the single-flight import lock, publishing progress over SSE and writing the
// dated import report when the pass finishes.
func (s *Server) runImport(sid int64) {
	s.importMu.Lock()
	if s.importBusy {
		s.importMu.Unlock()
		return
	}
	s.importBusy = true
	s.importTitle = ""
	s.importMu.Unlock()
	s.publishImportStatus()

	go func() {
		defer func() {
			s.importMu.Lock()
			s.importBusy = false
			s.importTitle = ""
			s.importMu.Unlock()
			s.publishImportStatus()
		}()

		sess, ok, err := s.st.GetImportSession(sid)
		if err != nil || !ok {
			log.Printf("import: load session %d: %v", sid, err)
			return
		}
		books, err := calibre.Read(sess.LibraryPath)
		if err != nil {
			log.Printf("import: read library %q: %v", sess.LibraryPath, err)
			return
		}
		books = s.importFilter().Apply(books)
		im := s.buildImport(sess)
		proc, err := s.st.ProcessedUUIDs()
		if err != nil {
			log.Printf("import: processed uuids: %v", err)
			return
		}
		units := importer.GroupWorkUnits(books, proc)
		if err := im.Run(sid, units); err != nil {
			log.Printf("import: run: %v", err)
			return
		}
		items, err := s.st.ListImportItems(sid)
		if err != nil {
			return
		}
		if _, sum, err := importer.WriteReport(sess.StagingDir, vault.Today(), items); err == nil {
			s.st.LogEvent("import", fmt.Sprintf("Calibre import: %d matched, %d unmatched, %d errored",
				sum.Matched, sum.Unmatched, sum.Errored))
		}
	}()
}

// buildImport assembles an Import over the live backends: OpenLibrary (ISBN +
// title), Lubimyczytać, and the jnovels scraper for both matching and the
// series volume-count scrape; the DupIndex is a snapshot of the tracked vault.
func (s *Server) buildImport(sess store.ImportSession) *importer.Import {
	// The stored provider.Provider is *OLClient underneath, which carries the
	// ISBN path the interface omits; degrade to title-only if it somehow isn't.
	var ol importer.OLSearcher
	if o, ok := s.ol.(importer.OLSearcher); ok {
		ol = o
	}
	resolver := sources.NewResolver(s.st)
	scrape := func(link string) (int, string) {
		nd, err := s.sc.NovelData(link, resolver.For(link))
		if err != nil {
			return 0, ""
		}
		return nd.Volumes, nd.Description
	}
	// Description fallback for a matched regular book whose Calibre record has no
	// comments: pull the blurb off the resolved source page (OpenLibrary for
	// English, Lubimyczytać for Polish). Best-effort — any failure leaves it blank.
	bookDesc := func(kind importer.Kind, link, workID string) string {
		switch kind {
		case importer.KindPolish:
			if s.lc != nil && link != "" {
				if b, err := s.lc.BookDetail(link); err == nil {
					return b.Description
				}
			}
		case importer.KindEnglish:
			if o, ok := s.ol.(interface {
				WorkDetail(string) (provider.Work, error)
			}); ok && workID != "" {
				if w, err := o.WorkDetail(workID); err == nil {
					return w.Description
				}
			}
		}
		return ""
	}
	progress := func(done, total int, title string) {
		s.importMu.Lock()
		s.importTitle = title
		s.importMu.Unlock()
		s.publishImportStatus()
	}
	return &importer.Import{
		Store:                 s.st,
		Matcher:               importer.New(ol, s.lc, s.sc, importMinGap*1_000_000), // ms → ns
		Writer:                importer.NewWriter(sess.StagingDir, vault.Today()),
		Dup:                   s.buildDupIndex(),
		Today:                 vault.Today(),
		ScrapeSeries:          scrape,
		ScrapeBookDescription: bookDesc,
		Progress:              progress,
	}
}

// buildDupIndex snapshots the tracked #Book/#LightNovel notes so the import can
// flag a staged note that duplicates an existing one. A scan failure degrades to
// an empty index (no dup flags) rather than aborting the import.
func (s *Server) buildDupIndex() *importer.DupIndex {
	entries, err := vault.ScanRoots(ScanRoots(s.cfg, s.st))
	if err != nil {
		return importer.NewDupIndex(nil)
	}
	return importer.NewDupIndex(entries)
}

// effectiveCalibreLibraryPath / effectiveImportStagingDir resolve the two import
// settings, the staging dir made absolute against the vault so it lands outside
// the scan roots wherever the vault lives.
func (s *Server) effectiveCalibreLibraryPath() string {
	return s.effective("calibre_library_path", s.cfg.CalibreLibraryPath)
}

func (s *Server) effectiveImportStagingDir() string {
	dir := s.effective("import_staging_dir", s.cfg.ImportStagingDir)
	if dir == "" {
		dir = "_CalibreImport"
	}
	return vault.ResolvePath(s.effective("vault_dir", s.cfg.VaultDir), dir)
}

// importFilter builds the identifier filter from the current settings, applied
// (before grouping) to both the dry-run preview and the real run so an
// owner-scoped library imports only the wanted books.
func (s *Server) importFilter() importer.ImportFilter {
	return importer.ImportFilter{
		Field:          s.effective("import_filter_field", s.cfg.ImportFilterField),
		Values:         importer.SplitFilterValues(s.effective("import_filter_values", s.cfg.ImportFilterValues)),
		IncludeMissing: s.effective("import_filter_include_missing", "") == "1",
	}
}
