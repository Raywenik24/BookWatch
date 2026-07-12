// Package server exposes the HTTP API + embedded web UI. Viewing is open;
// write endpoints require the shared password (see auth).
package server

import (
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bookwatch/internal/buildinfo"
	"bookwatch/internal/config"
	"bookwatch/internal/notes"
	"bookwatch/internal/provider"
	"bookwatch/internal/reading"
	"bookwatch/internal/scheduler"
	"bookwatch/internal/scraper"
	"bookwatch/internal/service"
	"bookwatch/internal/sources"
	"bookwatch/internal/sse"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

//go:embed web/index.html
var webFS embed.FS

// maxAPIBodyBytes caps inbound request bodies on write routes. Every JSON
// payload here (a URL, a few rules, a handful of settings) is tiny; 1 MiB is
// already generous and stops a client streaming an unbounded body.
const maxAPIBodyBytes = 1 << 20

type Server struct {
	cfg   config.Config
	st    *store.Store
	sc    *scraper.Client
	sched *scheduler.Scheduler
	ol    provider.Provider
	gb    *provider.GBClient
	gr    *provider.GRClient
	lc    *provider.LCClient

	// stream fans live check-status events out to connected clients (#47), so
	// the progress bar updates on push instead of polling /api/status.
	stream *sse.Broker

	coverMu  sync.Mutex
	coverIdx map[string]string // basename → abs path, lazy vault-wide cover index
	coverAt  time.Time         // when coverIdx was last built (for TTL invalidation)

	// importMu guards the single-flight Calibre import run (#75) and its live
	// progress, published over the same SSE stream as the check run. The done/
	// total figures come from the persisted item states (see importStatusPayload),
	// so only the in-flight unit's title needs tracking here.
	importMu    sync.Mutex
	importBusy  bool
	importTitle string
}

// coverIdxTTL is how long the vault-wide cover index is reused before a rebuild,
// so covers added after startup eventually resolve without a restart. A var so
// tests can force a rebuild.
var coverIdxTTL = 5 * time.Minute

func New(cfg config.Config, st *store.Store, sc *scraper.Client, sched *scheduler.Scheduler, ol provider.Provider, gb *provider.GBClient, gr *provider.GRClient, lc *provider.LCClient) *Server {
	s := &Server{cfg: cfg, st: st, sc: sc, sched: sched, ol: ol, gb: gb, gr: gr, lc: lc, stream: sse.New()}
	// Push a fresh status frame on every scheduler state change (start, each
	// book, finish) so subscribers see progress the instant it moves.
	sched.OnChange(func() { s.stream.Publish("status", s.statusPayload()) })
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/books", s.handleBooks)
	mux.HandleFunc("GET /api/updates", s.handleUpdates)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/stream", s.auth(s.stream.ServeHTTP))
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /api/sources", s.handleSources)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("GET /api/cover/{id}", s.handleCover)
	mux.HandleFunc("GET /api/gb/cover", s.handleGBCover)
	mux.HandleFunc("GET /api/ol/authors", s.handleOLAuthors)
	mux.HandleFunc("GET /api/ol/authors/{id}/works", s.handleOLAuthorWorks)
	mux.HandleFunc("GET /api/ol/search", s.handleOLSearch)
	mux.HandleFunc("GET /api/lc/search", s.handleLCSearch)
	mux.HandleFunc("GET /api/lc/book/detail", s.handleLCBookDetail)
	mux.HandleFunc("GET /api/ol/work/{id}", s.handleOLWork)
	mux.HandleFunc("GET /api/ol/work/{id}/detail", s.handleOLWorkDetail)
	mux.HandleFunc("GET /api/ol/works/{id}/editions", s.handleOLWorkEditions)
	mux.HandleFunc("GET /api/trackers", s.handleListTrackers)
	mux.HandleFunc("GET /api/releases", s.handleReleases)
	mux.HandleFunc("GET /api/releases/dismissed", s.handleDismissedReleases)
	mux.HandleFunc("GET /api/reading/completed", s.handleReadingCompleted)
	mux.HandleFunc("GET /api/reading/queue", s.handleReadingState)
	mux.HandleFunc("GET /api/reading/next-volume", s.handleReadingNextVolume)

	mux.HandleFunc("POST /api/check", s.auth(s.handleCheck))
	mux.HandleFunc("POST /api/vault/refresh", s.auth(s.handleVaultRefresh))
	mux.HandleFunc("POST /api/apply", s.auth(s.handleApply))
	mux.HandleFunc("POST /api/releases/add", s.auth(s.handleAddReleases))
	mux.HandleFunc("POST /api/releases/dismiss", s.auth(s.handleDismissReleases))
	mux.HandleFunc("POST /api/releases/undismiss", s.auth(s.handleUndismissReleases))
	mux.HandleFunc("POST /api/books", s.auth(s.handleAddBook))
	mux.HandleFunc("POST /api/books/{id}/complete", s.auth(s.handleMarkCompleted))
	mux.HandleFunc("POST /api/reading/start", s.auth(s.handleReadingStart))
	mux.HandleFunc("POST /api/reading/queue", s.auth(s.handleReadingQueueAdd))
	mux.HandleFunc("POST /api/reading/{id}/start", s.auth(s.handleReadingItemStart))
	mux.HandleFunc("PUT /api/reading/queue", s.auth(s.handleReadingReorder))
	mux.HandleFunc("DELETE /api/reading/{id}", s.auth(s.handleReadingDelete))
	mux.HandleFunc("PUT /api/reading/completed", s.auth(s.handleEditCompleted))
	mux.HandleFunc("DELETE /api/reading/completed", s.auth(s.handleDeleteCompleted))
	mux.HandleFunc("GET /api/books/{id}/note", s.handleReadNote)
	mux.HandleFunc("PUT /api/books/{id}/note", s.auth(s.handleEditNote))
	mux.HandleFunc("POST /api/books/{id}/cover", s.authLimited(s.handleSetCover, maxCoverUploadBytes))
	mux.HandleFunc("DELETE /api/books/{id}", s.auth(s.handleDeleteBook))
	mux.HandleFunc("POST /api/sources", s.auth(s.handleUpsertSource))
	mux.HandleFunc("DELETE /api/sources/{id}", s.auth(s.handleDeleteSource))
	mux.HandleFunc("PUT /api/sources/{id}/rules", s.auth(s.handleSetRules))
	mux.HandleFunc("POST /api/sources/test", s.auth(s.handleTest))
	mux.HandleFunc("POST /api/scrape/preview", s.auth(s.handleScrapePreview))
	mux.HandleFunc("PUT /api/settings", s.auth(s.handleSetSettings))
	mux.HandleFunc("POST /api/vault/setup", s.auth(s.handleVaultSetup))
	mux.HandleFunc("GET /api/vault/resolve", s.handleVaultResolve)
	mux.HandleFunc("GET /api/import/calibre/status", s.handleImportStatus)
	mux.HandleFunc("POST /api/import/calibre/preview", s.auth(s.handleImportPreview))
	mux.HandleFunc("POST /api/import/calibre", s.auth(s.handleImportStart))
	mux.HandleFunc("POST /api/import/calibre/stop", s.auth(s.handleImportStop))
	mux.HandleFunc("POST /api/import/calibre/retry", s.auth(s.handleImportRetry))
	mux.HandleFunc("POST /api/import/calibre/start-over", s.auth(s.handleImportStartOver))
	mux.HandleFunc("POST /api/import/calibre/finalize", s.auth(s.handleImportFinalize))
	mux.HandleFunc("GET /api/import/calibre/review", s.auth(s.handleReviewList))
	mux.HandleFunc("GET /api/import/calibre/review/item", s.auth(s.handleReviewItem))
	mux.HandleFunc("POST /api/import/calibre/review/pick", s.auth(s.handleReviewPick))
	mux.HandleFunc("POST /api/import/calibre/review/pull", s.auth(s.handleReviewPull))
	mux.HandleFunc("PUT /api/import/calibre/review/item", s.auth(s.handleReviewEdit))
	mux.HandleFunc("POST /api/import/calibre/review/cover", s.authLimited(s.handleReviewCover, maxCoverUploadBytes))
	mux.HandleFunc("POST /api/import/calibre/review/accept", s.auth(s.handleReviewAccept))
	mux.HandleFunc("POST /api/import/calibre/review/reject", s.auth(s.handleReviewReject))
	mux.HandleFunc("POST /api/import/calibre/review/accept-clean", s.auth(s.handleReviewAcceptClean))
	mux.HandleFunc("POST /api/trackers", s.auth(s.handleUpsertTracker))
	mux.HandleFunc("DELETE /api/trackers/{id}", s.auth(s.handleDeleteTracker))
	mux.HandleFunc("PUT /api/trackers/{id}/baseline", s.auth(s.handleUpdateBaseline))
	return secure(logging(mux))
}

// ── middleware ────────────────────────────────────────────────

// auth guards a write route and caps its body at maxAPIBodyBytes — every JSON
// write route decodes a tiny payload, so a 1 MiB cap is generous.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return s.authLimited(h, maxAPIBodyBytes)
}

// authLimited is auth with a custom body cap. The cover-upload route sends an
// image, not JSON, so it needs a much larger cap than the default — and because
// MaxBytesReader can only tighten, not loosen, the cap has to be set here rather
// than re-wrapped inside the handler.
func (s *Server) authLimited(h http.HandlerFunc, maxBody int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		// Header only — a query-param token would leak the password into proxy
		// access logs, browser history, and Referer headers (and weakens CSRF
		// posture, since a custom header can't be set cross-origin without a
		// preflight). The embedded UI only ever sends the header.
		token := r.Header.Get("X-BookWatch-Token")
		// Constant-time compare so the response timing doesn't leak how many
		// leading bytes of the password matched.
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Password)) != 1 {
			writeJSON(w, http.StatusUnauthorized, errBody("unauthorized"))
			return
		}
		h(w, r)
	}
}

// secure sets defense-in-depth response headers. The embedded UI is a single
// page with inline <style>, inline <script>, and inline event handlers, so the
// CSP must allow 'unsafe-inline' for style/script; everything else is locked to
// 'self' (covers come from /api/cover). External links are navigations, which
// CSP doesn't restrict, so they keep working.
func secure(next http.Handler) http.Handler {
	// OpenLibrary covers 302-redirect to archive.org and then to a regional
	// data node (iaNNN.us.archive.org), so all three hosts must be allowed or
	// the browser blocks the redirected image. Google Books is the first cover
	// fallback; Goodreads supplies the clustered-cover backfill (#40), and serves
	// its covers from the Amazon media CDN (m.media-amazon.com). Lubimyczytać
	// covers (Polish clustering + the add-a-book Polish fallback, #60) come from
	// its static host (s.lubimyczytac.pl).
	const csp = "default-src 'self'; img-src 'self' data: " +
		"https://covers.openlibrary.org https://archive.org https://*.us.archive.org " +
		"https://books.google.com https://lh3.googleusercontent.com " +
		"https://*.media-amazon.com https://*.lubimyczytac.pl; " +
		"style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; " +
		"base-uri 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// ── read handlers ─────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Serve the SPA shell for any non-API GET path so a refresh or bookmark on a
	// client route (/books, /updates, …) lands on the app, which then reads
	// location.pathname to activate the right tab (#62). Unmatched /api/ paths
	// fall through to here too — those are genuine 404s.
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The UI is embedded in the binary, so "refresh after rebuild" only works if
	// the browser doesn't serve a cached copy of the previous build's HTML.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

func (s *Server) handleBooks(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListBooks()
	respond(w, v, err)
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListUpdates(100)
	respond(w, v, err)
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListReleases(100)
	respond(w, v, err)
}

func (s *Server) handleDismissedReleases(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListDismissedReleases(100)
	respond(w, v, err)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListRuns(50)
	respond(w, v, err)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListEvents(100)
	respond(w, v, err)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusPayload())
}

// statusPayload is the shared check-status shape served by GET /api/status and
// pushed over /api/stream. A CountPending error degrades to pending:0 rather
// than failing — a live progress frame shouldn't die on a transient DB read,
// and the pending count self-corrects on the next frame.
func (s *Server) statusPayload() map[string]any {
	cur, total, title := s.sched.Progress()
	pending, _ := s.st.CountPending()
	return map[string]any{
		"busy":          s.sched.Busy(),
		"current":       cur,
		"total":         total,
		"current_title": title,
		"pending":       pending,
	}
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": buildinfo.Version,
		"commit":  buildinfo.Commit,
		"date":    buildinfo.Date,
	})
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListSources()
	respond(w, v, err)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	saved, err := s.st.AllSettings()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"effective": map[string]string{
			"vault_dir":            s.effective("vault_dir", s.cfg.VaultDir),
			"scan_root":            s.effective("scan_root", s.cfg.ScanRoot),
			"new_note_dir":         s.effective("new_note_dir", s.cfg.NewNoteDir),
			"attachments_dir":      s.effective("attachments_dir", s.cfg.AttachmentsDir),
			"book_scan_root":       s.effectiveBookScanRoot(),
			"book_new_note_dir":    s.effectiveBookNewNoteDir(),
			"book_attachments_dir": s.effectiveBookAttachmentsDir(),
			"reading_log_path":     s.effective("reading_log_path", s.cfg.ReadingLogPath),
			"ln_check_cron":                 s.effective("ln_check_cron", s.cfg.CheckCron),
			"tracker_check_cron":            s.effective("tracker_check_cron", s.cfg.TrackerCron),
			"calibre_library_path":          s.effective("calibre_library_path", s.cfg.CalibreLibraryPath),
			"import_staging_dir":            s.effective("import_staging_dir", s.cfg.ImportStagingDir),
			"import_filter_field":           s.effective("import_filter_field", s.cfg.ImportFilterField),
			"import_filter_values":          s.effective("import_filter_values", s.cfg.ImportFilterValues),
			"import_filter_include_missing": s.effective("import_filter_include_missing", ""),
		},
		"overrides": saved,
	})
}

// handleCover streams a book's cover from the effective attachments dir.
// Open like the other reads. filepath.Base guards against path escapes.
func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	cover, err := s.st.BookCover(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if cover == "" {
		http.NotFound(w, r)
		return
	}
	p := s.resolveCover(cover)
	if p == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	ct := mime.TypeByExtension(filepath.Ext(p))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	io.Copy(w, f)
}

// ── write handlers ────────────────────────────────────────────

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !s.sched.Trigger("api") {
		writeJSON(w, http.StatusConflict, errBody("a check is already running"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleVaultRefresh reconciles the DB with what's on disk right now — no
// scraping, no author polling — so books whose notes vanished between full
// checks stop lingering in the UI (#57).
func (s *Server) handleVaultRefresh(w http.ResponseWriter, r *http.Request) {
	if s.sched.Busy() {
		writeJSON(w, http.StatusConflict, errBody("a check is running — try again when it finishes"))
		return
	}
	res, err := service.RefreshVault(s.st, ScanRoots(s.cfg, s.st))
	if err != nil {
		writeErr(w, err)
		return
	}
	if res.Added > 0 || res.Removed > 0 {
		s.st.LogEvent("refresh", fmt.Sprintf("Refreshed vault info: +%d added, -%d removed", res.Added, res.Removed))
	}
	writeJSON(w, http.StatusOK, res)
}

// handleApply writes the selected pending bumps to the vault (last check's
// stored numbers — no re-scrape), bumps each book, and stamps the updates
// applied. Only ticked rows are touched (issue #36) — an empty/missing ids
// list applies nothing.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if s.sched.Busy() {
		writeJSON(w, http.StatusConflict, errBody("a check is running — try again when it finishes"))
		return
	}
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("bad body"))
			return
		}
	}
	res, err := service.ApplyPending(s.st, vault.Today(), body.IDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	if res.Applied > 0 || res.Failed > 0 {
		s.st.LogEvent("apply", fmt.Sprintf("Applied %d update(s) to Obsidian, %d failed", res.Applied, res.Failed))
	}
	writeJSON(w, http.StatusOK, res)
}

// releaseAddResult is one release's outcome from handleAddReleases.
type releaseAddResult struct {
	Title string `json:"title"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

// handleAddReleases turns the selected surfaced releases into #Book notes
// (status Backlog), the same catalog flow as the add-a-book page, then marks
// each created and seen so the tracker poll never re-surfaces it.
func (s *Server) handleAddReleases(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effectiveBookNewNoteDir(),
		AttachmentsDir: s.effectiveBookAttachmentsDir(),
	}
	created, failed := 0, 0
	var results []releaseAddResult
	for _, id := range body.IDs {
		rel, err := s.st.GetRelease(id)
		if err != nil {
			failed++
			results = append(results, releaseAddResult{Error: err.Error()})
			continue
		}
		olURL := "https://openlibrary.org/works/" + rel.WorkID
		coverURL, description, releasedEN := rel.CoverURL, "", ""
		if work, err := s.ol.WorkDetail(rel.WorkID); err == nil {
			if coverURL == "" {
				coverURL = provider.SelectCover(work, "eng")
			}
			description = work.Description
			releasedEN = yearStr(work.FirstPubYear)
		}
		res, err := notes.CreateBook(opts, s.st, rel.Title, rel.Author, olURL, rel.WorkID, coverURL, "Backlog", releasedEN, description)
		if err != nil {
			failed++
			results = append(results, releaseAddResult{Title: rel.Title, Error: err.Error()})
			continue
		}
		if _, err := s.st.UpsertBook(res.Title, olURL, res.Path, 0, res.Cover, "Backlog", nil, "book", rel.Author); err != nil {
			failed++
			results = append(results, releaseAddResult{Title: rel.Title, Error: err.Error()})
			continue
		}
		if err := s.st.MarkReleaseCreated(rel.ID); err != nil {
			writeErr(w, err)
			return
		}
		if err := s.st.AddSeenWork(rel.TrackerID, rel.WorkID); err != nil {
			writeErr(w, err)
			return
		}
		created++
		results = append(results, releaseAddResult{Title: res.Title, Path: res.Path})
	}
	if created > 0 || failed > 0 {
		s.st.LogEvent("apply", fmt.Sprintf("Created %d release note(s), %d failed", created, failed))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": created, "failed": failed, "results": results,
	})
}

func (s *Server) handleDismissReleases(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	for _, id := range body.IDs {
		if err := s.st.DismissRelease(id); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

func (s *Server) handleUndismissReleases(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	for _, id := range body.IDs {
		if err := s.st.UndismissRelease(id); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "undismissed"})
}

// addBookBody covers both add flows: the LN scrape flow (url only) and the
// catalog flow (kind="book", everything else) for an OpenLibrary candidate
// the user picked or resolved by URL.
type addBookBody struct {
	URL string `json:"url"`

	Kind                   string `json:"kind"`   // "" (default: LN scrape) | "book"
	Source                 string `json:"source"` // "" (OpenLibrary) | "lc" (Lubimyczytać, #60)
	Title                  string `json:"title"`
	Author                 string `json:"author"`
	AuthorOLKey            string `json:"author_ol_key"`
	WorkID                 string `json:"work_id"`
	OLURL                  string `json:"ol_url"`
	CoverURL               string `json:"cover_url"`
	Year                   int    `json:"year"`
	Status                 string `json:"status"`
	WatchAuthor            bool   `json:"watch_author"`
	CatalogLanguage        string `json:"catalog_language"`
	WatchPolishTranslation bool   `json:"watch_pl_translation"`
}

func (s *Server) handleAddBook(w http.ResponseWriter, r *http.Request) {
	var body addBookBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	if body.Kind == "book" {
		if body.Source == "lc" {
			s.addBookFromLC(w, body)
			return
		}
		s.addBookFromCatalog(w, body)
		return
	}
	if body.URL == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effective("new_note_dir", s.cfg.NewNoteDir),
		AttachmentsDir: s.effective("attachments_dir", s.cfg.AttachmentsDir),
	}
	rl := sources.NewResolver(s.st).For(body.URL)
	res, err := notes.Create(opts, s.sc, s.st, rl, body.URL, body.Status)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, notes.ErrDuplicate) || errors.Is(err, notes.ErrNoteExists) {
			code = http.StatusConflict
		}
		writeJSON(w, code, errBody(err.Error()))
		return
	}
	if _, err := s.st.UpsertBook(res.Title, body.URL, res.Path, res.Volumes, res.Cover, "", nil, "ln", ""); err != nil {
		writeErr(w, err)
		return
	}
	s.st.LogEvent("add", fmt.Sprintf("Added %q (%s)", res.Title, body.URL))
	writeJSON(w, http.StatusCreated, map[string]any{
		"title": res.Title, "volumes": res.Volumes, "path": res.Path,
	})
}

// maxBackfillISBNs caps how many same-language ISBNs the #42 author/
// description backfill tries — the matcher stops at the first that
// resolves, so a long list only adds dead lookups.
const maxBackfillISBNs = 5

// addBookFromCatalog creates a #Book note from an OpenLibrary candidate (no
// scraping) — issue #34's add-a-book path. The cover is the catalog-language
// edition (falls back to any edition with a cover); if watch_author is set,
// a Watchlist tracker is created baselined on this book.
func (s *Server) addBookFromCatalog(w http.ResponseWriter, body addBookBody) {
	if body.Title == "" || body.WorkID == "" || body.OLURL == "" {
		writeJSON(w, http.StatusBadRequest, errBody("title, work_id, ol_url required"))
		return
	}
	lang := body.CatalogLanguage
	if lang == "" {
		lang = "eng"
	}
	status := body.Status
	if status == "" {
		status = "Completed"
	}

	// Released EN is the candidate's first_publish_year (the picker already
	// showed it); fall back to the work record when the candidate carried none
	// — it's the work's original-language first-publish year, not strictly the
	// English edition (#51/#2).
	coverURL, description, releasedEN := "", "", yearStr(body.Year)
	if work, err := s.ol.WorkDetail(body.WorkID); err == nil {
		coverURL = provider.SelectCover(work, lang)
		description = work.Description
		if releasedEN == "" {
			releasedEN = yearStr(work.FirstPubYear)
		}
		// OL's own index can be missing author/description on a sparse
		// translation work record (#42) — try the same matchers the
		// baseline picker uses to backfill from Goodreads/Lubimyczytać.
		// Only the catalog-language edition's own ISBNs are offered: a
		// work's other editions resolve to *their* language's blurb on
		// Goodreads (verified live on Glen Cook's "The White Rose" —
		// an English work whose editions.json lists a German ISBN
		// first), so mixing every edition's ISBNs risks backfilling the
		// wrong language entirely.
		if body.Author == "" || description == "" {
			isbns := provider.EditionISBNs(work.Editions, lang)
			if len(isbns) > maxBackfillISBNs {
				isbns = isbns[:maxBackfillISBNs]
			}
			var matchers []provider.Matcher
			if s.gr != nil {
				matchers = append(matchers, s.gr)
			}
			if s.lc != nil {
				matchers = append(matchers, s.lc)
			}
			for _, m := range matchers {
				if body.Author != "" && description != "" {
					break
				}
				if gm := m.MatchWork(work.Title, body.Author, isbns); gm.Found {
					if body.Author == "" && gm.Author != "" {
						body.Author = gm.Author
					}
					if description == "" && gm.Description != "" {
						description = gm.Description
					}
				}
			}
		}
	}

	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effectiveBookNewNoteDir(),
		AttachmentsDir: s.effectiveBookAttachmentsDir(),
	}
	res, err := notes.CreateBook(opts, s.st, body.Title, body.Author, body.OLURL, body.WorkID, coverURL, status, releasedEN, description)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, notes.ErrDuplicate) || errors.Is(err, notes.ErrNoteExists) {
			code = http.StatusConflict
		}
		writeJSON(w, code, errBody(err.Error()))
		return
	}
	if _, err := s.st.UpsertBook(res.Title, body.OLURL, res.Path, 0, res.Cover, status, nil, "book", body.Author); err != nil {
		writeErr(w, err)
		return
	}
	s.st.LogEvent("add", fmt.Sprintf("Added %q (%s)", res.Title, body.OLURL))

	watched := false
	if body.WatchAuthor && body.AuthorOLKey != "" {
		baselineDate := ""
		if body.Year > 0 {
			baselineDate = strconv.Itoa(body.Year)
		}
		watchPL := body.WatchPolishTranslation && lang != "pol"
		if _, err := s.st.UpsertTracker("author", body.Author, body.AuthorOLKey, body.WorkID, baselineDate, lang, watchPL); err != nil {
			log.Printf("watch author after book add: %v", err)
		} else {
			watched = true
			s.st.LogEvent("tracker_add", fmt.Sprintf("Watching author %q (baseline %s)", body.Author, res.Title))
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"title": res.Title, "path": res.Path, "watched": watched,
	})
}

// addBookFromLC creates a #Book note from a Lubimyczytać candidate the user
// picked when OpenLibrary had no match (issue #60). Unlike the OL catalog path
// there's no work id / author-watch: LC books have no OpenLibrary identity, so
// the note keys on LC's own ids — Link = the /ksiazka page URL (the dedup key,
// carried in OLURL), Work ID = the LC book id (WorkID). The /ksiazka page is
// fetched once for the blurb + Polish publication date (→ Released EN); the
// cover the user saw in the picker is downloaded as usual, falling back to the
// detail page's own cover.
func (s *Server) addBookFromLC(w http.ResponseWriter, body addBookBody) {
	if body.Title == "" || body.WorkID == "" || body.OLURL == "" {
		writeJSON(w, http.StatusBadRequest, errBody("title, work_id, ol_url required"))
		return
	}
	if s.lc == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("Lubimyczytać source unavailable"))
		return
	}
	status := body.Status
	if status == "" {
		status = "Completed"
	}

	coverURL, description, releasedEN := body.CoverURL, "", ""
	if book, err := s.lc.BookDetail(body.OLURL); err == nil {
		description = book.Description
		releasedEN = book.ReleaseDate
		if coverURL == "" {
			coverURL = book.CoverURL
		}
	}

	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effectiveBookNewNoteDir(),
		AttachmentsDir: s.effectiveBookAttachmentsDir(),
	}
	res, err := notes.CreateBook(opts, s.st, body.Title, body.Author, body.OLURL, body.WorkID, coverURL, status, releasedEN, description)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, notes.ErrDuplicate) || errors.Is(err, notes.ErrNoteExists) {
			code = http.StatusConflict
		}
		writeJSON(w, code, errBody(err.Error()))
		return
	}
	if _, err := s.st.UpsertBook(res.Title, body.OLURL, res.Path, 0, res.Cover, status, nil, "book", body.Author); err != nil {
		writeErr(w, err)
		return
	}
	s.st.LogEvent("add", fmt.Sprintf("Added %q (%s)", res.Title, body.OLURL))
	writeJSON(w, http.StatusCreated, map[string]any{
		"title": res.Title, "path": res.Path, "watched": false,
	})
}

// handleDeleteBook untracks a book: removes its DB row only. The vault note and
// cover are left untouched, so the book reappears on the next check if the note
// still exists. With ?hard=1 (#58) it instead hard-deletes the note and cover
// from the vault before untracking.
func (s *Server) handleDeleteBook(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("hard") == "1" {
		s.handleDeleteBookHard(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	title, _ := s.st.BookTitle(id)
	if err := s.st.DeleteBook(id); err != nil {
		writeErr(w, err)
		return
	}
	if title != "" {
		s.st.LogEvent("untrack", fmt.Sprintf("Untracked %q", title))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "untracked"})
}

// handleDeleteBookHard hard-deletes a book's note and cover attachment from the
// vault (#58), then untracks it. No trash folder — this is a permanent delete;
// the vault's own version history (e.g. OneDrive) is the safety net.
func (s *Server) handleDeleteBookHard(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	if err := os.Remove(ref.Path); err != nil && !os.IsNotExist(err) {
		writeErr(w, err)
		return
	}
	if ref.Cover != "" {
		opts := s.noteOptions(ref.Kind)
		attachAbs := vault.ResolvePath(opts.VaultDir, opts.AttachmentsDir)
		if err := os.Remove(filepath.Join(attachAbs, ref.Cover)); err != nil && !os.IsNotExist(err) {
			writeErr(w, err)
			return
		}
	}
	if err := s.st.DeleteBook(ref.ID); err != nil {
		writeErr(w, err)
		return
	}
	s.st.LogEvent("delete", fmt.Sprintf("Deleted %q (note + cover)", ref.Title))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── reading log (#63) ─────────────────────────────────────────

// handleReadingCompleted serves the parsed completed-reads log plus the
// per-(title, volume) re-read counts. Read-only and open like the other feeds;
// powers the Reading tab's Completed view + `×N` badges (#64). An unconfigured
// or missing log is not an error — it just yields an empty history.
func (s *Server) handleReadingCompleted(w http.ResponseWriter, r *http.Request) {
	logPath := s.effectiveReadingLogPath()
	if logPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "reads": []reading.Read{}, "reread": []rereadCount{}})
		return
	}
	reads, err := reading.ParseFile(logPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	var reread []rereadCount
	for k, n := range reading.ReReadCounts(reads) {
		if n >= 2 {
			reread = append(reread, rereadCount{Title: k.Title, Volume: k.Volume, Count: n})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "reads": reads, "reread": reread})
}

// rereadCount is one re-read (count ≥ 2) surfaced to the UI as a badge.
type rereadCount struct {
	Title  string `json:"title"`
	Volume string `json:"volume"`
	Count  int    `json:"count"`
}

// ── reading queue + currently-reading (#64) ───────────────────

// handleReadingState serves the Reading tab's two live lists: what's in progress
// and what's queued (both joined to their book note). Read-only and open like
// the other feeds; the Completed list comes from /api/reading/completed.
func (s *Server) handleReadingState(w http.ResponseWriter, r *http.Request) {
	reading, err := s.st.ListReadingItems("reading")
	if err != nil {
		writeErr(w, err)
		return
	}
	queue, err := s.st.ListReadingItems("queue")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reading": nonNilItems(reading),
		"queue":   nonNilItems(queue),
	})
}

func nonNilItems(v []store.ReadingItem) []store.ReadingItem {
	if v == nil {
		return []store.ReadingItem{}
	}
	return v
}

// handleReadingNextVolume suggests the next LN volume to read for a tracked book
// (issue #64 point 4): one past the highest volume already in the reading log,
// capped at the note's total Volumes. Open (read-only). A #Book or an
// unconfigured log yields volume "" — there's nothing to suggest.
func (s *Server) handleReadingNextVolume(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("book_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad book_id"))
		return
	}
	ref, ok, err := s.st.BookByID(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok || ref.Kind == "book" {
		writeJSON(w, http.StatusOK, map[string]string{"volume": ""})
		return
	}
	capVol, readVol := s.bookVolumes(id)
	reads, _ := reading.ParseFile(s.effectiveReadingLogPath())
	logMax := reading.MaxVolume(reads, ref.Title)
	start := readVol
	if logMax > start {
		start = logMax
	}
	n := start + 1
	if capVol > 0 && n > capVol {
		n = capVol
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"volume": strconv.Itoa(n),
	})
}

// bookVolumes returns a tracked book's total Volumes and its Read Volumes
// (0 if unknown) — used to seed the next-volume suggestion (issue #68) with
// volumes already read even when the reading log has no rows for it yet.
func (s *Server) bookVolumes(id int64) (volumes, readVolumes int) {
	books, err := s.st.ListBooks()
	if err != nil {
		return 0, 0
	}
	for _, b := range books {
		if b.ID == id {
			if b.ReadVolumes != nil {
				readVolumes = *b.ReadVolumes
			}
			return b.Volumes, readVolumes
		}
	}
	return 0, 0
}

// readingTarget decodes {book_id, volume, start_date} and resolves the book,
// guarding that only a tracked note can be queued/started (#64). start_date is
// optional (only the start path uses it — the user may have begun before opening
// the app). Writes the error response and returns ok=false on any failure.
func (s *Server) readingTarget(w http.ResponseWriter, r *http.Request) (store.BookRef, string, string, bool) {
	var body struct {
		BookID    int64  `json:"book_id"`
		Volume    string `json:"volume"`
		StartDate string `json:"start_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return store.BookRef{}, "", "", false
	}
	ref, ok, err := s.st.BookByID(body.BookID)
	if err != nil {
		writeErr(w, err)
		return store.BookRef{}, "", "", false
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("no such tracked book"))
		return store.BookRef{}, "", "", false
	}
	volume := strings.TrimSpace(body.Volume)
	if ref.Kind == "book" {
		volume = "" // a whole #Book has no volume, whatever the client sent
	}
	return ref, volume, strings.TrimSpace(body.StartDate), true
}

// handleReadingStart puts a tracked book/LN volume into currently-reading,
// stamping the start date (#64 point 3). The date defaults to today but the
// client may supply an earlier one when the read began before the app was opened.
func (s *Server) handleReadingStart(w http.ResponseWriter, r *http.Request) {
	ref, volume, startDate, ok := s.readingTarget(w, r)
	if !ok {
		return
	}
	if startDate == "" {
		startDate = vault.Today()
	}
	id, err := s.st.StartReadingItem(ref.ID, volume, startDate)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.st.LogEvent("reading", fmt.Sprintf("Started reading %s", readingUnit(ref.Title, volume)))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "reading"})
}

// handleReadingQueueAdd appends a tracked book/LN volume to the read-next queue.
func (s *Server) handleReadingQueueAdd(w http.ResponseWriter, r *http.Request) {
	ref, volume, _, ok := s.readingTarget(w, r)
	if !ok {
		return
	}
	id, err := s.st.QueueReadingItem(ref.ID, volume)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "queued"})
}

// handleReadingItemStart moves a queued row into currently-reading, stamping
// today as the start date.
func (s *Server) handleReadingItemStart(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	if err := s.st.StartQueuedItem(id, vault.Today()); err != nil {
		writeErr(w, err)
		return
	}
	if it, ok, _ := s.st.GetReadingItem(id); ok {
		s.st.LogEvent("reading", fmt.Sprintf("Started reading %s", readingUnit(it.Title, it.Volume)))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reading"})
}

// handleReadingReorder persists a new queue order from a drag-and-drop reorder.
func (s *Server) handleReadingReorder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	if err := s.st.ReorderQueue(body.IDs); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}

// handleReadingDelete drops a queued or currently-reading row (without logging a
// completion — that's the ✓ Done path).
func (s *Server) handleReadingDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	if err := s.st.DeleteReadingItem(id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// handleEditCompleted rewrites one existing row in the reading log (#64). The
// body is {index, title, volume, start, end, unknown, abandoned}: index is the
// row's position in file order (as served by /api/reading/completed), title is
// the expected [[wikilink]] target (a stale-index guard), and the rest rebuild
// the row the same way a fresh completion would.
func (s *Server) handleEditCompleted(w http.ResponseWriter, r *http.Request) {
	logPath := s.effectiveReadingLogPath()
	if logPath == "" {
		writeJSON(w, http.StatusBadRequest, errBody("reading log note is not configured"))
		return
	}
	var body struct {
		Index     *int   `json:"index"`
		Title     string `json:"title"`
		Volume    string `json:"volume"`
		Start     string `json:"start"`
		End       string `json:"end"`
		Unknown   bool   `json:"unknown"`
		Abandoned bool   `json:"abandoned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Index == nil || body.Title == "" {
		writeJSON(w, http.StatusBadRequest, errBody("index and title required"))
		return
	}
	row := reading.NewCompletedRow(body.Title, body.Volume, body.Start, body.End, body.Unknown)
	if body.Abandoned {
		row = reading.NewAbandonedRow(body.Title, body.Volume, body.Start)
	}
	if err := reading.UpdateReadAt(logPath, *body.Index, body.Title, row); err != nil {
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
		return
	}
	s.st.LogEvent("read", fmt.Sprintf("Edited log entry %s", readingUnit(body.Title, body.Volume)))
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleDeleteCompleted removes one row from the reading log by its file-order
// index (?index=), guarded by ?title= against a stale index.
func (s *Server) handleDeleteCompleted(w http.ResponseWriter, r *http.Request) {
	logPath := s.effectiveReadingLogPath()
	if logPath == "" {
		writeJSON(w, http.StatusBadRequest, errBody("reading log note is not configured"))
		return
	}
	index, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad index"))
		return
	}
	title := r.URL.Query().Get("title")
	if err := reading.DeleteReadAt(logPath, index, title); err != nil {
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
		return
	}
	s.st.LogEvent("read", fmt.Sprintf("Deleted log entry %q", title))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// readingUnit names a reading unit for an event message ("Title" or "Title vol N").
func readingUnit(title, volume string) string {
	if volume != "" {
		return fmt.Sprintf("%q vol %s", title, volume)
	}
	return fmt.Sprintf("%q", title)
}

// handleMarkCompleted logs a completed read for a tracked book. The body is
// {volume, start, end, unknown}: volume names the LN volume (blank for a #Book),
// start/end are the picker dates, and unknown ("I don't remember") blanks the
// dates + YYYYMM. A #Book completion also flips its note Status to Completed.
func (s *Server) handleMarkCompleted(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	logPath := s.effectiveReadingLogPath()
	if logPath == "" {
		writeJSON(w, http.StatusBadRequest, errBody("reading log note is not configured — set it in Settings → Paths"))
		return
	}
	var body struct {
		Volume        string `json:"volume"`
		Start         string `json:"start"`
		End           string `json:"end"`
		Unknown       bool   `json:"unknown"`
		Abandoned     bool   `json:"abandoned"`       // log a `----` end marker + Drop the #Book (#64)
		ReadingItemID int64  `json:"reading_item_id"` // optional: the currently-reading row to clear (#64)
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("bad body"))
			return
		}
	}

	result, err := service.MarkCompleted(logPath, ref.Path, ref.Kind, ref.Title, body.Volume, body.Start, body.End, body.Unknown, body.Abandoned)
	if err != nil {
		writeErr(w, err)
		return
	}

	// ✓ Done / Abandon on a currently-reading item: the read is now in the log,
	// so drop its queue/reading row (best-effort — the log write already succeeded).
	if body.ReadingItemID != 0 {
		s.st.DeleteReadingItem(body.ReadingItemID)
	}

	// The series continues — queue the next volume so the user doesn't have to
	// go find it themselves (#67). Best-effort, same as the delete above.
	if result.NextVolume != "" {
		s.st.QueueReadingItem(ref.ID, result.NextVolume)
	}

	// The note on disk changed (Status and/or Read Volumes) — resync the DB row
	// so the Books feed and randomizer reflect it without waiting for the next
	// scan (#67).
	if n, err := vault.ReadNote(ref.Path); err == nil {
		s.resyncNote(ref.Link, ref.Path, n)
	}
	unit := ref.Title
	if body.Volume != "" {
		unit += " vol " + body.Volume
	}
	verb, status := "Marked", "completed"
	if body.Abandoned {
		verb, status = "Abandoned", "abandoned"
	}
	s.st.LogEvent("read", fmt.Sprintf("%s %q", verb, unit))
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "reread_count": result.RereadCount})
}

// maxCoverUploadBytes caps an uploaded cover image (matches notes' download cap).
const maxCoverUploadBytes = 16 << 20 // 16 MiB

// ── note view/edit (#55) ──────────────────────────────────────

// lookupBook resolves the {id} path value to a tracked note on disk, writing
// the appropriate error response (and returning ok=false) on any failure so the
// note handlers can bail out cleanly.
func (s *Server) lookupBook(w http.ResponseWriter, r *http.Request) (store.BookRef, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return store.BookRef{}, false
	}
	ref, ok, err := s.st.BookByID(id)
	if err != nil {
		writeErr(w, err)
		return store.BookRef{}, false
	}
	if !ok || ref.Path == "" {
		writeJSON(w, http.StatusNotFound, errBody("no note for this book"))
		return store.BookRef{}, false
	}
	if _, err := os.Stat(ref.Path); err != nil {
		writeJSON(w, http.StatusNotFound, errBody("note file missing on disk"))
		return store.BookRef{}, false
	}
	return ref, true
}

// noteOptions returns the vault paths for a note of the given kind (book notes
// use the separate Book new-note/attachments folders when configured, #50).
func (s *Server) noteOptions(kind string) notes.Options {
	o := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effective("new_note_dir", s.cfg.NewNoteDir),
		AttachmentsDir: s.effective("attachments_dir", s.cfg.AttachmentsDir),
	}
	if kind == "book" {
		o.NewNoteDir = s.effectiveBookNewNoteDir()
		o.AttachmentsDir = s.effectiveBookAttachmentsDir()
	}
	return o
}

// resyncNote refreshes the DB row for a note from its file after an edit. The
// note is keyed on its (unchanged) Link, so a rename just updates title+path.
func (s *Server) resyncNote(link, path string, n vault.Note) error {
	var rv *int
	if n.HasReadVolumes {
		v := n.ReadVolumes
		rv = &v
	}
	_, err := s.st.UpsertBook(n.Title, link, path, n.Volumes, n.Cover, n.Status, rv, n.Kind, n.Author)
	return err
}

// ensureKindTag guarantees the note's defining tag (#Book / #LightNovel) stays
// in the tag list — Scan and kind detection both key on it, so an edit that
// dropped it would silently untrack the note. The kind tag is placed first;
// remaining user tags follow, de-duplicated case-insensitively.
func ensureKindTag(kind string, tags []string) []string {
	kt := "LightNovel"
	if kind == "book" {
		kt = "Book"
	}
	out := []string{kt}
	seen := map[string]bool{strings.ToLower(kt): true}
	for _, t := range tags {
		norm := strings.TrimPrefix(strings.TrimSpace(t), "#")
		if norm == "" || seen[strings.ToLower(norm)] {
			continue
		}
		seen[strings.ToLower(norm)] = true
		out = append(out, norm)
	}
	return out
}

func (s *Server) handleReadNote(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	n, err := vault.ReadNote(ref.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

// handleEditNote applies the edit fields present in the JSON body (any subset of
// description, status, tags, released_en, title). Frontmatter/body edits run
// first; a title change (file rename + cover follow) runs last since it moves
// the path. The DB row is then re-synced from the file.
func (s *Server) handleEditNote(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	var body struct {
		Description *string   `json:"description"`
		Status      *string   `json:"status"`
		Tags        *[]string `json:"tags"`
		ReleasedEN  *string   `json:"released_en"`
		Title       *string   `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}

	path := ref.Path
	if body.Status != nil {
		if err := vault.UpdateStatus(path, *body.Status); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Tags != nil {
		if err := vault.UpdateTags(path, ensureKindTag(ref.Kind, *body.Tags)); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.ReleasedEN != nil {
		if err := vault.UpdateReleasedEN(path, *body.ReleasedEN); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Description != nil {
		if err := vault.UpdateDescription(path, *body.Description); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Title != nil && strings.TrimSpace(*body.Title) != "" {
		newPath, _, err := notes.RenameNote(s.noteOptions(ref.Kind), path, ref.Cover, *body.Title)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, notes.ErrNoteExists) {
				code = http.StatusConflict
			}
			writeJSON(w, code, errBody(err.Error()))
			return
		}
		path = newPath
		s.bustCoverIndex()
	}

	n, err := vault.ReadNote(path)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.resyncNote(ref.Link, path, n); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

// handleSetCover replaces a note's cover from either an uploaded image
// (multipart/form-data, field "cover") or a pasted URL (JSON {"url":…}). The new
// file lands in the note's attachments dir named cover_<title><ext>, the note's
// Cover field + embed are rewritten, and any superseded file is removed.
func (s *Server) handleSetCover(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	opts := s.noteOptions(ref.Kind)
	attachAbs := vault.ResolvePath(opts.VaultDir, opts.AttachmentsDir)

	var ext string
	var save func(dest string) error

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		// Body cap is applied by authLimited (maxCoverUploadBytes); a file over it
		// fails the multipart parse here.
		file, hdr, err := r.FormFile("cover")
		if err != nil {
			msg := "no cover file"
			if strings.Contains(err.Error(), "too large") {
				msg = "cover image is too large (max 16 MiB)"
			}
			writeJSON(w, http.StatusBadRequest, errBody(msg))
			return
		}
		defer file.Close()
		ext = strings.ToLower(filepath.Ext(hdr.Filename))
		if !isImageExt(ext) {
			writeJSON(w, http.StatusBadRequest, errBody("cover must be an image (jpg, png, gif, webp)"))
			return
		}
		save = func(dest string) error { return notes.SaveCoverBytes(dest, file) }
	} else {
		var b struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.URL == "" {
			writeJSON(w, http.StatusBadRequest, errBody("missing url"))
			return
		}
		if !notes.IsValidURL(b.URL) {
			writeJSON(w, http.StatusBadRequest, errBody("invalid url"))
			return
		}
		ext = notes.CoverExt(b.URL)
		url := b.URL
		save = func(dest string) error { return notes.DownloadCover(url, dest) }
	}
	if ext == "" {
		ext = ".jpg"
	}

	coverName := notes.CoverName(ref.Title, ext)
	if err := save(filepath.Join(attachAbs, coverName)); err != nil {
		writeErr(w, err)
		return
	}
	// Drop a superseded cover whose name differs (e.g. a different extension).
	if ref.Cover != "" && ref.Cover != coverName {
		os.Remove(filepath.Join(attachAbs, ref.Cover))
	}
	if err := vault.UpdateCover(ref.Path, coverName); err != nil {
		writeErr(w, err)
		return
	}
	s.bustCoverIndex()

	n, err := vault.ReadNote(ref.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.resyncNote(ref.Link, ref.Path, n); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

// bustCoverIndex forces the vault-wide cover index to rebuild on next lookup, so
// a just-added/renamed cover resolves immediately instead of after the TTL.
func (s *Server) bustCoverIndex() {
	s.coverMu.Lock()
	s.coverIdx = nil
	s.coverMu.Unlock()
}

func isImageExt(ext string) bool {
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	}
	return false
}

func (s *Server) handleUpsertSource(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name, Domain, Strategy string
		Enabled                bool
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Domain == "" {
		writeJSON(w, http.StatusBadRequest, errBody("name + domain required"))
		return
	}
	id, err := s.st.UpsertSource(b.Name, b.Domain, b.Strategy, b.Enabled)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	if err := s.st.DeleteSource(id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleSetRules(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	var b struct {
		Rules []store.Rule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	for _, rule := range b.Rules {
		if err := s.st.UpsertRule(id, rule.Field, rule.Selector, rule.Regex, rule.Attr); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleTest runs rules against a URL and returns what would be extracted,
// without saving anything. Rules in the body override the resolved ones.
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	var b struct {
		URL   string       `json:"url"`
		Rules []store.Rule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.URL == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	var rl scraper.Rules
	if len(b.Rules) > 0 {
		rl = sources.BuildRules(b.Rules)
	} else {
		rl = sources.NewResolver(s.st).For(b.URL)
	}
	nd, err := s.sc.NovelData(b.URL, rl)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, nd)
}

// handleScrapePreview dry-runs the LN scrape for the add-a-book preview (#52):
// scrape the URL with its resolved source rules and return the data
// (title, volumes, cover, description) without writing a note. The note is
// created only when the user confirms — via the normal POST /api/books path,
// which re-scrapes (the scrape is idempotent, so no server-side caching).
func (s *Server) handleScrapePreview(w http.ResponseWriter, r *http.Request) {
	var b struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.URL == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	if !notes.IsValidURL(b.URL) {
		writeJSON(w, http.StatusBadRequest, errBody("invalid url"))
		return
	}
	rl := sources.NewResolver(s.st).For(b.URL)
	nd, err := s.sc.NovelData(b.URL, rl)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	// The scraped cover lives on the source's own host, which the page CSP's
	// img-src doesn't (and can't — it's arbitrary) whitelist, so the browser
	// would block an <img src> pointing straight at it. Inline it as a data:
	// URI instead (CSP allows data:) by fetching it here through the same
	// SSRF-guarded client the real note-creation cover download uses. Best
	// effort: a fetch failure just yields no preview cover, same as before.
	writeJSON(w, http.StatusOK, map[string]any{
		"title":       nd.Title,
		"volumes":     nd.Volumes,
		"cover_url":   nd.CoverURL,
		"cover_data":  coverDataURI(nd.CoverURL),
		"description": nd.Description,
	})
}

// maxPreviewCoverBytes caps the inlined preview cover. Cover art is well under
// this; the guard keeps a hostile source from streaming a huge body into memory.
const maxPreviewCoverBytes = 8 << 20 // 8 MiB

// coverDataURI fetches a scraped cover URL through the SSRF-guarded client and
// returns it as a base64 data: URI, or "" on any failure (the caller treats an
// absent cover the same as a failed one).
func coverDataURI(coverURL string) string {
	if coverURL == "" {
		return ""
	}
	client := scraper.NewGuardedHTTPClient(15 * time.Second)
	resp, err := client.Get(coverURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPreviewCoverBytes))
	if err != nil || len(data) == 0 {
		return ""
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = http.DetectContentType(data)
	}
	if !strings.HasPrefix(ct, "image/") {
		return ""
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ── Google Books proxy (open — public API, key is optional) ───

func (s *Server) handleGBCover(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	author := r.URL.Query().Get("author")
	if title == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing title"))
		return
	}
	// Google Books only here — this lazy per-tile path has just a title+author,
	// and Goodreads can't be searched by title (its /search is WAF-blocked). The
	// Goodreads cover backfill happens in the clustered works endpoint instead,
	// where the OL work's ISBNs are available (#40).
	cover := s.gb.CoverURL(title, author)
	writeJSON(w, http.StatusOK, map[string]string{"cover": cover})
}

// ── OpenLibrary proxy (open — OL is a public API) ─────────────

func (s *Server) handleOLAuthors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing q"))
		return
	}
	results, err := s.ol.AuthorSearch(q)
	respond(w, results, err)
}

// grClusterBudget caps how many Goodreads ISBN lookups one author-works request
// may trigger. Each is a polite ~0.7s-throttled fetch of an ~800 KB book page,
// so a full refine on a prolific author runs on the order of a minute —
// acceptable because the picker fires it in the background over the fast view
// and the per-ISBN cache makes repeat visits free.
//
// lcClusterBudget is smaller: a Polish author routes nearly every survivor to
// the Lubimyczytać pass, so an unbounded LC pass is what originally turned a
// Polish-author refine into minutes. Now that a lookup is a single search
// request (no follow-up book-page fetch), the budget can afford to be a bit
// higher and still land well under the old ceiling; spent coverless-works-first,
// it backfills the most visible Polish covers first. Survivors past either
// budget keep their title-normalization grouping.
const (
	grClusterBudget = 60
	lcClusterBudget = 25
)

func (s *Server) handleOLAuthorWorks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	works, err := s.ol.AuthorWorks(id)
	if err != nil {
		respond(w, works, err)
		return
	}
	// ?cluster=1&author=<name>: collapse translation/edition dupes via Goodreads
	// work-clustering and backfill missing covers (#40). The picker fires this as
	// a background refine after the fast title-norm view, so a slow scrape never
	// blocks the first render. Without it (or without a Goodreads client) the
	// pure pass-1 title-normalization result is returned.
	author := r.URL.Query().Get("author")
	var gr, lc provider.Matcher
	var unsafeCovers bool
	if r.URL.Query().Get("cluster") == "1" {
		if s.gr != nil {
			gr = s.gr
		}
		if s.lc != nil {
			lc = s.lc
		}
		// Opt-in (Settings): when neither OL nor a guarded match yields a cover,
		// borrow a Goodreads cover by ISBN alone, flagged unverified (#41).
		if v, ok, _ := s.st.GetSetting("unsafe_cover_match"); ok && v == "1" {
			unsafeCovers = true
		}
	}
	respond(w, provider.ClusterWorks(works, author, gr, lc, grClusterBudget, lcClusterBudget, unsafeCovers), nil)
}

func (s *Server) handleOLSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing q"))
		return
	}
	results, err := s.ol.SearchByTitle(q)
	respond(w, results, err)
}

// ── Lubimyczytać proxy (open — the add-a-book Polish fallback, #60) ───

// handleLCSearch surfaces Lubimyczytać search hits as add-a-book candidates when
// OpenLibrary has nothing (issue #60). Open like the OL search proxy. A nil LC
// client (Polish source disabled) returns an empty list, so the UI just reports
// no matches.
func (s *Server) handleLCSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing q"))
		return
	}
	if s.lc == nil {
		respond(w, []provider.LCSearchResult{}, nil)
		return
	}
	respond(w, s.lc.SearchCandidates(q), nil)
}

// handleLCBookDetail returns a Lubimyczytać book's blurb + Polish publication
// date for the add-a-book preview, fetched lazily when the user picks an LC
// candidate — the counterpart to handleOLWorkDetail. The ?url= is the picked
// candidate's own /ksiazka page URL (only its path is used server-side).
// "released" carries the Polish publication date string (mapped to the note's
// Released EN field).
func (s *Server) handleLCBookDetail(w http.ResponseWriter, r *http.Request) {
	pageURL := r.URL.Query().Get("url")
	if pageURL == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	if s.lc == nil {
		respond(w, map[string]any{"description": "", "released": ""}, nil)
		return
	}
	book, err := s.lc.BookDetail(pageURL)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{
		"description": book.Description,
		"released":    book.ReleaseDate,
	}, nil)
}

// handleOLWork resolves a single work by ID — the pasted-URL path in the
// add-a-book flow, where the user already picked the exact work and a title
// search would be redundant.
func (s *Server) handleOLWork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cand, err := s.ol.WorkByID(id)
	respond(w, cand, err)
}

// handleOLWorkDetail returns a work's blurb (and first-publish year) for the
// add-a-book preview, which fetches it lazily when a candidate is picked and
// shows a spinner meanwhile (#51/#2). Kept separate from handleOLWork: that
// path resolves author identity for a pasted URL, this one just needs the
// description the search index doesn't carry.
func (s *Server) handleOLWorkDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	work, err := s.ol.WorkDetail(id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{
		"title":       work.Title,
		"released":    work.FirstPubYear,
		"description": work.Description,
	}, nil)
}

// yearStr renders a publish year for a note's "Released EN" field — blank for a
// zero/unknown year rather than a literal "0".
func yearStr(y int) string {
	if y <= 0 {
		return ""
	}
	return strconv.Itoa(y)
}

// handleOLWorkEditions resolves a work's real per-edition data, since the
// bulk author-works search only exposes an aggregated per-work language tag
// that can pick the wrong translation, or even the wrong language entirely
// when a work bundles many languages' editions under one title (#45). The
// baseline picker calls this lazily per tile, alongside its existing
// cover-backfill pass, rather than upfront for the whole author.
//
// With ?lang=xx (the currently selected catalog language): looks for an
// edition actually tagged xx and, if found, returns matched=true plus that
// edition's own title/cover — e.g. work "Season of Storms" has a "pol"
// edition titled "Sezon Burz", which is what the picker should show and
// filter on for a Polish tracker, not the work's default English title.
// matched=false tells the picker this work genuinely has no xx edition, so
// it should be dropped from that language's view rather than kept on a
// guess. Without ?lang=, falls back to a majority-vote language across all
// editions, used only for the "All languages" browse view's badge.
func (s *Server) handleOLWorkEditions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	eds, err := s.ol.WorkEditions(id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if lang := r.URL.Query().Get("lang"); lang != "" {
		ed, ok := provider.FindEdition(eds, lang)
		if !ok {
			respond(w, map[string]any{"matched": false}, nil)
			return
		}
		cover := ed.CoverURL
		if cover == "" {
			cover = provider.SelectCover(provider.Work{Editions: eds}, lang)
		}
		respond(w, map[string]any{
			"matched":   true,
			"language":  lang,
			"title":     ed.Title,
			"cover_url": cover,
		}, nil)
		return
	}
	respond(w, map[string]any{
		"matched":   true,
		"language":  provider.MajorityLanguage(eds),
		"cover_url": provider.SelectCover(provider.Work{Editions: eds}, ""),
	}, nil)
}

// ── trackers ───────────────────────────────────────────────────

func (s *Server) handleListTrackers(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListTrackers()
	respond(w, v, err)
}

func (s *Server) handleUpsertTracker(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name                   string `json:"name"`
		OLKey                  string `json:"ol_key"`
		BaselineWorkID         string `json:"baseline_work_id"`
		BaselineDate           string `json:"baseline_date"`
		CatalogLanguage        string `json:"catalog_language"`
		WatchPolishTranslation bool   `json:"watch_pl_translation"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.OLKey == "" {
		writeJSON(w, http.StatusBadRequest, errBody("name + ol_key required"))
		return
	}
	watchPL := b.WatchPolishTranslation && b.CatalogLanguage != "pol"
	id, err := s.st.UpsertTracker("author", b.Name, b.OLKey, b.BaselineWorkID, b.BaselineDate, b.CatalogLanguage, watchPL)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.st.LogEvent("tracker_add", fmt.Sprintf("Watching author %q (baseline %s)", b.Name, b.BaselineDate))
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleDeleteTracker(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	if err := s.st.DeleteTracker(id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleUpdateBaseline(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad id"))
		return
	}
	var b struct {
		BaselineWorkID         string `json:"baseline_work_id"`
		BaselineDate           string `json:"baseline_date"`
		CatalogLanguage        string `json:"catalog_language"`
		WatchPolishTranslation bool   `json:"watch_pl_translation"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	watchPL := b.WatchPolishTranslation && b.CatalogLanguage != "pol"
	if err := s.st.UpdateTrackerBaseline(id, b.BaselineWorkID, b.BaselineDate, b.CatalogLanguage, watchPL); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// pathSettingKeys are the settings whose value is a filesystem path — these
// get their separators normalized to native (backslash on Windows) on save,
// so the DB stops accumulating mixed slashes (issue #66).
var pathSettingKeys = map[string]bool{
	"vault_dir": true, "scan_root": true, "new_note_dir": true, "attachments_dir": true,
	"book_scan_root": true, "book_new_note_dir": true, "book_attachments_dir": true,
	"reading_log_path": true, "calibre_library_path": true, "import_staging_dir": true,
}

// cronSettingKeys are the settings holding a cron expression — validated with
// robfig/cron's standard parser before being persisted, and rescheduled live
// on the running scheduler so the edit takes effect without a restart (#80).
var cronSettingKeys = map[string]bool{"ln_check_cron": true, "tracker_check_cron": true}

func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var kv map[string]string
	if err := json.NewDecoder(r.Body).Decode(&kv); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	for k, v := range kv {
		if cronSettingKeys[k] {
			if err := scheduler.ValidateSpec(strings.TrimSpace(v)); err != nil {
				writeJSON(w, http.StatusBadRequest, errBody(fmt.Sprintf("%s: invalid cron expression: %v", k, err)))
				return
			}
		}
	}
	for k, v := range kv {
		if pathSettingKeys[k] {
			v = filepath.FromSlash(v)
		}
		if err := s.st.SetSetting(k, v); err != nil {
			writeErr(w, err)
			return
		}
	}
	if _, ok := kv["ln_check_cron"]; ok {
		if err := s.sched.RescheduleLN(s.effective("ln_check_cron", s.cfg.CheckCron)); err != nil {
			writeErr(w, err)
			return
		}
	}
	if _, ok := kv["tracker_check_cron"]; ok {
		if err := s.sched.RescheduleTracker(s.effective("tracker_check_cron", s.cfg.TrackerCron)); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleVaultSetup is the final confirm of the vault setup wizard (issue #65).
// It persists every chosen path to the settings table, then does all the disk
// writes at once: MkdirAll on each configured folder, the reading-log note
// (create-if-missing, with a table header), and a `.obsidian/` marker dir so
// Obsidian recognizes the folder as a vault. Every disk write is create-if-
// missing — existing files and folders are never clobbered.
func (s *Server) handleVaultSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VaultDir           string `json:"vault_dir"`
		ScanRoot           string `json:"scan_root"`
		NewNoteDir         string `json:"new_note_dir"`
		AttachmentsDir     string `json:"attachments_dir"`
		BookScanRoot       string `json:"book_scan_root"`
		BookNewNoteDir     string `json:"book_new_note_dir"`
		BookAttachmentsDir string `json:"book_attachments_dir"`
		ReadingLogPath     string `json:"reading_log_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	vaultDir := strings.TrimSpace(req.VaultDir)
	if vaultDir == "" {
		writeJSON(w, http.StatusBadRequest, errBody("vault root is required"))
		return
	}

	// Persist the chosen paths (blank sub-paths are allowed — they fall back to
	// the Light Novel field / server defaults via the effective* lookups).
	kv := map[string]string{
		"vault_dir":            filepath.FromSlash(vaultDir),
		"scan_root":            filepath.FromSlash(strings.TrimSpace(req.ScanRoot)),
		"new_note_dir":         filepath.FromSlash(strings.TrimSpace(req.NewNoteDir)),
		"attachments_dir":      filepath.FromSlash(strings.TrimSpace(req.AttachmentsDir)),
		"book_scan_root":       filepath.FromSlash(strings.TrimSpace(req.BookScanRoot)),
		"book_new_note_dir":    filepath.FromSlash(strings.TrimSpace(req.BookNewNoteDir)),
		"book_attachments_dir": filepath.FromSlash(strings.TrimSpace(req.BookAttachmentsDir)),
		"reading_log_path":     filepath.FromSlash(strings.TrimSpace(req.ReadingLogPath)),
	}
	for k, v := range kv {
		if err := s.st.SetSetting(k, v); err != nil {
			writeErr(w, err)
			return
		}
	}

	// MkdirAll the vault root and every configured sub-folder (relatives resolve
	// against the vault root; absolutes are used as-is).
	dirs := []string{vaultDir}
	for _, rel := range []string{
		kv["scan_root"], kv["new_note_dir"], kv["attachments_dir"],
		kv["book_scan_root"], kv["book_new_note_dir"], kv["book_attachments_dir"],
	} {
		if rel != "" {
			dirs = append(dirs, vault.ResolvePath(vaultDir, rel))
		}
	}
	dirs = append(dirs, filepath.Join(vaultDir, ".obsidian"))
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody("create folder "+d+": "+err.Error()))
			return
		}
	}

	// Reading-log note — create-if-missing, with a table header.
	if lp := kv["reading_log_path"]; lp != "" {
		if err := reading.EnsureLog(vault.ResolvePath(vaultDir, lp)); err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody("create reading log: "+err.Error()))
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleVaultResolve resolves a (possibly relative) vault path to an absolute
// one for display in the setup wizard (issue #81) — the wizard should never
// show ".\vault" when the server actually resolves it against its own CWD.
// Read-only and side-effect-free, so it doesn't need the password gate.
func (s *Server) handleVaultResolve(w http.ResponseWriter, r *http.Request) {
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	abs := dir
	if dir != "" {
		if a, err := filepath.Abs(filepath.FromSlash(dir)); err == nil {
			abs = a
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"abs": abs})
}

// ── helpers ───────────────────────────────────────────────────

// resolveCover maps a cover attachment filename to an absolute path. It checks
// the configured attachments dir first, then (Obsidian-style) the rest of the
// vault by basename — vaults often keep attachments in per-folder _attachments
// dirs rather than one fixed location. The vault-wide index is built lazily and
// cached; newly added covers land in the configured dir and hit the fast path.
func (s *Server) resolveCover(name string) string {
	name = filepath.Base(name)
	vaultDir := s.effective("vault_dir", s.cfg.VaultDir)
	direct := filepath.Join(vault.ResolvePath(vaultDir, s.effective("attachments_dir", s.cfg.AttachmentsDir)), name)
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	return s.coverIndex(vaultDir)[name]
}

// coverIndex returns the vault-wide basename→path index, rebuilding it when it's
// missing or older than coverIdxTTL. The vault walk runs WITHOUT the lock held,
// so concurrent cover requests aren't serialized behind a full-vault walk; the
// lock is only taken to read and to swap in the rebuilt map. A rare double-walk
// from two simultaneous misses is harmless (both produce the same map).
func (s *Server) coverIndex(vaultDir string) map[string]string {
	s.coverMu.Lock()
	idx := s.coverIdx
	fresh := idx != nil && time.Since(s.coverAt) < coverIdxTTL
	s.coverMu.Unlock()
	if fresh {
		return idx
	}

	built := indexVaultFiles(vaultDir) // walk off-lock

	s.coverMu.Lock()
	s.coverIdx, s.coverAt = built, time.Now()
	s.coverMu.Unlock()
	return built
}

// indexVaultFiles walks root once, mapping each file's basename to its path
// (first occurrence wins). Used as the Obsidian-style fallback cover lookup.
func indexVaultFiles(root string) map[string]string {
	idx := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if _, ok := idx[d.Name()]; !ok {
			idx[d.Name()] = p
		}
		return nil
	})
	return idx
}

// effective returns a settings-table override for key, else the cfg fallback.
func (s *Server) effective(key, fallback string) string {
	return effectiveSetting(s.st, key, fallback)
}

func effectiveSetting(st *store.Store, key, fallback string) string {
	if v, ok, _ := st.GetSetting(key); ok && v != "" {
		return v
	}
	return fallback
}

// effectiveBookScanRoot/NewNoteDir/AttachmentsDir mirror effective() for the
// #Book path fields, falling back to the Light Novel field when the Book one
// is blank — so an existing single-folder setup keeps working unchanged.
func (s *Server) effectiveBookScanRoot() string {
	if v := s.effective("book_scan_root", s.cfg.BookScanRoot); v != "" {
		return v
	}
	return s.effective("scan_root", s.cfg.ScanRoot)
}

func (s *Server) effectiveBookNewNoteDir() string {
	if v := s.effective("book_new_note_dir", s.cfg.BookNewNoteDir); v != "" {
		return v
	}
	return s.effective("new_note_dir", s.cfg.NewNoteDir)
}

func (s *Server) effectiveBookAttachmentsDir() string {
	if v := s.effective("book_attachments_dir", s.cfg.BookAttachmentsDir); v != "" {
		return v
	}
	return s.effective("attachments_dir", s.cfg.AttachmentsDir)
}

// effectiveReadingLogPath returns the absolute path to the reading log note
// (issue #63), resolved against the vault dir, or "" when no path is configured
// (the reading engine stays inert until the user sets one).
func (s *Server) effectiveReadingLogPath() string {
	p := s.effective("reading_log_path", s.cfg.ReadingLogPath)
	if p == "" {
		return ""
	}
	return vault.ResolvePath(s.effective("vault_dir", s.cfg.VaultDir), p)
}

// ScanRoots returns the effective Light Novel + Book scan roots (Book falls
// back to the LN root when unset), each resolved against the vault dir so a
// root entered relative to the vault (or as a full path) both work. Exported
// for the scheduler and CLI check, which run before/without a Server instance.
func ScanRoots(cfg config.Config, st *store.Store) []string {
	vaultDir := effectiveSetting(st, "vault_dir", cfg.VaultDir)
	ln := effectiveSetting(st, "scan_root", cfg.ScanRoot)
	book := effectiveSetting(st, "book_scan_root", cfg.BookScanRoot)
	if book == "" {
		book = ln
	}
	return []string{vault.ResolvePath(vaultDir, ln), vault.ResolvePath(vaultDir, book)}
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }
