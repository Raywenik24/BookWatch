package server

import (
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bookwatch/internal/notes"
	"bookwatch/internal/scraper"
	"bookwatch/internal/sources"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

// Per-volume LN note backfill (#90). Adding an LN series scrapes only the
// aggregate page (title + cover + volume count); this job fills in the rest —
// one #LNVolume note per volume, each with its own cover + description — so the
// Reading tab can show the *volume being read*'s cover instead of the series
// cover for 19 volumes straight.
//
// jnovels lists a series' volumes as bare download links (not per-volume pages),
// so each volume needs its own jnovels search + scrape (scraper.VolumeData). That
// is slow and best-effort, so the whole thing runs on a background goroutine with
// a politeness throttle, streaming progress over the SSE `backfill` channel. A
// volume that can't be resolved gets a minimal `#LNVolume/incomplete` placeholder
// (the issue's default (b) — an empty note to fill later), never blocking the run.

// maxBackfillVolumes caps how many volumes one job scrapes, so a bad volume count
// can't fan out into an unbounded burst of jnovels requests. Real LN series are
// well under this.
const maxBackfillVolumes = 120

// backfillThrottle is the polite gap between per-volume lookups. Each volume is a
// search + a page fetch + a cover download, so a slow drip keeps a long series
// from hammering jnovels. A var so tests can shrink it.
var backfillThrottle = 800 * time.Millisecond

// startVolumeBackfill launches — at most once per series at a time — a background
// job that writes an untracked #LNVolume note per volume of series. Returns
// immediately; progress flows over SSE. A no-op for a blank series, a
// non-positive volume count, or a series already being backfilled.
//
// series must be the sanitized series-note title (the one the note file is named
// after), so the `_volumes/<Series>/` folder the volumes land in matches what the
// reading-view cover lookup derives from that note's path. Only the in-memory
// dedupe key is apostrophe-normalized (jnovels' search needs that) — never the
// on-disk name.
func (s *Server) startVolumeBackfill(series, seriesLink string, volumes int, language string) {
	series = strings.TrimSpace(series)
	if series == "" || volumes <= 0 {
		return
	}
	if volumes > maxBackfillVolumes {
		volumes = maxBackfillVolumes
	}
	key := strings.ToLower(scraper.NormalizeApostrophes(series))

	s.backfillMu.Lock()
	if s.backfillActive[key] {
		s.backfillMu.Unlock()
		return
	}
	s.backfillActive[key] = true
	s.backfillMu.Unlock()

	go s.runVolumeBackfill(series, seriesLink, key, volumes, language)
}

func (s *Server) runVolumeBackfill(series, seriesLink, key string, volumes int, language string) {
	defer func() {
		s.backfillMu.Lock()
		delete(s.backfillActive, key)
		s.backfillMu.Unlock()
	}()

	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effective("new_note_dir", s.cfg.NewNoteDir),
		AttachmentsDir: s.effective("attachments_dir", s.cfg.AttachmentsDir),
	}
	rl := sources.NewResolver(s.st).For("https://jnovels.com/")

	resolved, incomplete := 0, 0
	alias := ""
	var missed []int
	s.publishBackfill(series, 0, volumes, resolved, incomplete, "running")

	// Pass 1: search each volume under the series' own title. Note the localized
	// alias title jnovels files the volumes under, discovered from a hit whose
	// series differs from ours ("Kumo Desu ga Nani ka" → "So I'm a Spider…").
	for v := 1; v <= volumes; v++ {
		// #108: derive the volume's jnovels URL straight from the series slug and
		// try it first. The title-search matcher can't tell an "Ex" spinoff from
		// the main series, but jnovels' slug pattern is exact — so this resolves
		// per-volume "Ex" posts the search misses. matched=false (a 404, e.g.
		// vol 1 = the series page, or an unscrapeable page) falls through to the
		// existing search chain unchanged.
		if written, matched := s.deriveVolume(opts, series, seriesLink, v, volumes, language, rl); matched {
			if written {
				resolved++
			}
		} else if nd, url, postSeries, err := s.sc.VolumeData(series, v, rl); err == nil {
			if s.writeVolume(opts, series, v, volumes, language, url, nd) {
				resolved++
			}
			if alias == "" && postSeries != "" && !scraper.SameSeries(series, postSeries) {
				alias = postSeries
			}
		} else {
			missed = append(missed, v)
		}
		s.publishBackfill(series, v, volumes, resolved, incomplete, "running")
		if v < volumes {
			time.Sleep(backfillThrottle)
		}
	}

	// Pass 2: jnovels' search sometimes won't surface a volume under the series'
	// own title but finds it under the localized alias (verified: "…Volume 4"
	// returns nothing, "So I'm a Spider So What Volume 4" returns the post). Retry
	// the misses under the discovered alias.
	if alias != "" && len(missed) > 0 {
		var still []int
		for _, v := range missed {
			if nd, url, _, err := s.sc.VolumeData(alias, v, rl); err == nil && s.writeVolume(opts, series, v, volumes, language, url, nd) {
				resolved++
			} else {
				still = append(still, v)
			}
			s.publishBackfill(series, volumes, volumes, resolved, incomplete, "running")
			time.Sleep(backfillThrottle)
		}
		missed = still
	}

	// Pass 3: finalize the rest. Volume 1 falls back to the series page (on jnovels
	// the series page *is* volume 1); everything else becomes an incomplete
	// placeholder to fill later.
	for _, v := range missed {
		if v == 1 && seriesLink != "" {
			if snd, serr := s.sc.NovelData(seriesLink, rl); serr == nil && snd.CoverURL != "" {
				if s.writeVolume(opts, series, 1, volumes, language, seriesLink, snd) {
					resolved++
				}
				continue
			}
		}
		if _, cerr := notes.CreateLNVolume(opts, series, v, volumes, language, "", "", "", "", true); cerr == nil {
			incomplete++
		}
	}

	// Point the series note at its volume notes (idempotent), then refresh covers.
	seriesNote := filepath.Join(vault.ResolvePath(opts.VaultDir, opts.NewNoteDir), notes.Sanitize(series, false)+".md")
	if err := notes.AppendVolumeLinks(seriesNote, series, volumes); err != nil {
		log.Printf("backfill: append volume links for %q: %v", series, err)
	}
	s.bustCoverIndex()     // so the new volume covers resolve without waiting for the TTL
	s.bustBackfillStatus() // refresh the Books-grid attention badges
	s.st.LogEvent("backfill", fmt.Sprintf("Volume backfill for %q: %d resolved, %d incomplete (of %d)",
		series, resolved, incomplete, volumes))
	s.publishBackfill(series, volumes, volumes, resolved, incomplete, "done")
}

// writeVolume creates a resolved #LNVolume note from a scrape. It reports true
// only when a new note was actually written — an already-existing note
// (ErrNoteExists) or a write error yields false, so the caller doesn't
// double-count it as freshly resolved.
func (s *Server) writeVolume(opts notes.Options, series string, volume, total int, language, url string, nd scraper.NovelData) bool {
	_, err := notes.CreateLNVolume(opts, series, volume, total, language, url, nd.CoverURL, "", nd.Description, false)
	return err == nil
}

// deriveVolume implements #108's derive-URL-first attempt: it builds the volume's
// jnovels post URL directly from the series slug (which encodes the exact series,
// "Ex" spinoffs included, where a title search can't) and scrapes it. matched is
// true when the derived URL is a real, scrapeable volume post — signalling the
// caller to skip the title-search fallback; written is true only when a fresh note
// was actually created (for the resolved counter). A 404 (e.g. vol 1, which on
// jnovels *is* the series page) or an unscrapeable page yields matched=false, so
// the existing search chain runs unchanged.
func (s *Server) deriveVolume(opts notes.Options, series, seriesLink string, v, volumes int, language string, rl scraper.Rules) (written, matched bool) {
	url := deriveVolumeURL(seriesLink, v)
	if url == "" {
		return false, false
	}
	nd, err := s.sc.NovelData(url, rl)
	if err != nil || nd.CoverURL == "" {
		return false, false
	}
	return s.writeVolume(opts, series, v, volumes, language, url, nd), true
}

// deriveVolumeURL constructs the jnovels per-volume post URL for volume v from a
// series page URL, using the same slug jnovels keys posts on: strip the series
// page's trailing "-light-novel"/"-epub"/"-pdf" markers (via seriesKey) and append
// "-volume-<v>-epub". Returns "" when seriesLink carries no usable slug. jnovels'
// slug pattern is stable and verified (#108); a series filed under a different
// pattern just 404s the derived URL and the caller falls back to search.
func deriveVolumeURL(seriesLink string, v int) string {
	trimmed := strings.TrimRight(strings.TrimSpace(seriesLink), "/")
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return ""
	}
	slug := seriesKey(seriesLink)
	if slug == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s-volume-%d-epub/", trimmed[:i], slug, v)
}

// publishBackfill pushes a backfill-progress frame to SSE subscribers.
func (s *Server) publishBackfill(series string, done, total, resolved, incomplete int, state string) {
	s.stream.Publish("backfill", map[string]any{
		"series":     series,
		"done":       done,
		"total":      total,
		"resolved":   resolved,
		"incomplete": incomplete,
		"state":      state,
	})
}

// backfillRunning reports whether a backfill job is currently in flight for a
// series (its sanitized title), so the UI can disable the trigger + show progress.
func (s *Server) backfillRunning(series string) bool {
	key := strings.ToLower(scraper.NormalizeApostrophes(strings.TrimSpace(series)))
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	return s.backfillActive[key]
}

// lnSeriesRef resolves a book id to its LN series note: the series name (its note
// basename, which the _volumes folder is keyed on), the note path, the aggregate
// Link, and the tracked volume count. It writes the error response and returns
// ok=false when the book isn't an LN series with a readable note.
func (s *Server) lnSeriesRef(w http.ResponseWriter, id int64) (b store.BookRef, series string, volumes int, ok bool) {
	b, found, err := s.st.BookByID(id)
	if err != nil {
		writeErr(w, err)
		return b, "", 0, false
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errBody("no such book"))
		return b, "", 0, false
	}
	if b.Kind != "ln" || strings.TrimSpace(b.Path) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("volume backfill is only for light-novel series"))
		return b, "", 0, false
	}
	n, err := vault.ReadNote(b.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("could not read the series note"))
		return b, "", 0, false
	}
	series = strings.TrimSuffix(filepath.Base(b.Path), filepath.Ext(b.Path))
	return b, series, n.Volumes, true
}

// handleVolumeStates lists the per-volume backfill status of an LN series (for
// the note-modal reviewer), plus whether a job is currently running. Open (read).
func (s *Server) handleVolumeStates(w http.ResponseWriter, r *http.Request) {
	b, series, volumes, ok := s.lnSeriesRef(w, idFromPath(r))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volumes": volumes,
		"running": s.backfillRunning(series),
		"states":  notes.VolumeStates(b.Path, series, volumes),
	})
}

// handleBackfillVolumes runs a retroactive backfill on an existing series: it
// clears the incomplete placeholders (so their slots are re-attempted) and starts
// the same background job the add flow uses. Resolved/hand-edited volume notes are
// left untouched (the job skips notes already on disk).
func (s *Server) handleBackfillVolumes(w http.ResponseWriter, r *http.Request) {
	b, series, volumes, ok := s.lnSeriesRef(w, idFromPath(r))
	if !ok {
		return
	}
	if volumes <= 0 {
		writeJSON(w, http.StatusBadRequest, errBody("the series note has no volume count to back-fill"))
		return
	}
	removed, _ := notes.RemoveIncompleteVolumes(b.Path)
	s.startVolumeBackfill(series, b.Link, volumes, "")
	s.st.LogEvent("backfill", fmt.Sprintf("Retroactive volume backfill for %q (cleared %d incomplete)", series, removed))
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "cleared": removed, "volumes": volumes})
}

// handleResolveVolume fills one volume from a jnovels URL the user pasted (the
// reviewer's "paste the exact link" path for a miss): it scrapes the page and
// writes/overwrites that volume's #LNVolume note with the cover + description +
// link. A volume that already has a *resolved* note is left alone (409) so a
// hand-edited note isn't clobbered.
func (s *Server) handleResolveVolume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	if !notes.IsValidURL(body.URL) {
		writeJSON(w, http.StatusBadRequest, errBody("invalid url"))
		return
	}
	b, series, volumes, ok := s.lnSeriesRef(w, idFromPath(r))
	if !ok {
		return
	}
	volume, _ := strconv.Atoi(r.PathValue("n"))
	if volume < 1 || (volumes > 0 && volume > volumes) {
		writeJSON(w, http.StatusBadRequest, errBody("volume out of range"))
		return
	}
	if notes.VolumeStateOf(b.Path, series, volume) == "resolved" {
		writeJSON(w, http.StatusConflict, errBody("that volume already has a filled note — edit it directly instead"))
		return
	}
	rl := sources.NewResolver(s.st).For(body.URL)
	nd, err := s.sc.NovelData(body.URL, rl)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("could not read that page: "+err.Error()))
		return
	}
	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effective("new_note_dir", s.cfg.NewNoteDir),
		AttachmentsDir: s.effective("attachments_dir", s.cfg.AttachmentsDir),
	}
	// Drop the incomplete placeholder so CreateLNVolume writes a fresh resolved note.
	os.Remove(longPathServer(notes.VolumePath(b.Path, series, volume)))
	res, err := notes.CreateLNVolume(opts, series, volume, volumes, "", body.URL, nd.CoverURL, "", nd.Description, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.bustCoverIndex()
	s.bustBackfillStatus()
	s.st.LogEvent("backfill", fmt.Sprintf("Filled %s volume %d from %s", series, volume, body.URL))
	writeJSON(w, http.StatusOK, map[string]any{"status": "resolved", "cover": res.Cover, "title": res.Title})
}

// handleFillVolume writes a volume note from values the user typed by hand — the
// reviewer's fallback for a volume that simply doesn't exist on jnovels
// (mirroring the Calibre import's manual edit). It accepts either a multipart form
// (cover file upload) or JSON (cover_url), plus description / released / link, and
// overwrites the incomplete placeholder with a filled #LNVolume note.
func (s *Server) handleFillVolume(w http.ResponseWriter, r *http.Request) {
	b, series, volumes, ok := s.lnSeriesRef(w, idFromPath(r))
	if !ok {
		return
	}
	volume, _ := strconv.Atoi(r.PathValue("n"))
	if volume < 1 || (volumes > 0 && volume > volumes) {
		writeJSON(w, http.StatusBadRequest, errBody("volume out of range"))
		return
	}
	if notes.VolumeStateOf(b.Path, series, volume) == "resolved" {
		writeJSON(w, http.StatusConflict, errBody("that volume already has a filled note — edit it directly instead"))
		return
	}

	var description, releasedEN, link, coverURL string
	var coverFile multipart.File
	var coverExt string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if file, hdr, err := r.FormFile("cover"); err == nil {
			defer file.Close()
			ext := strings.ToLower(filepath.Ext(hdr.Filename))
			if !isImageExt(ext) {
				writeJSON(w, http.StatusBadRequest, errBody("cover must be an image (jpg, png, gif, webp)"))
				return
			}
			coverFile, coverExt = file, ext
		}
		description = r.FormValue("description")
		releasedEN = r.FormValue("released")
		link = r.FormValue("link")
	} else {
		var body struct {
			Description string `json:"description"`
			Released    string `json:"released"`
			Link        string `json:"link"`
			CoverURL    string `json:"cover_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("bad body"))
			return
		}
		description, releasedEN, link, coverURL = body.Description, body.Released, body.Link, strings.TrimSpace(body.CoverURL)
	}

	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effective("new_note_dir", s.cfg.NewNoteDir),
		AttachmentsDir: s.effective("attachments_dir", s.cfg.AttachmentsDir),
	}
	// Save the cover (upload or URL) into the LN attachments dir, named for the volume.
	coverName := ""
	if coverFile != nil || coverURL != "" {
		attachAbs := vault.ResolvePath(opts.VaultDir, opts.AttachmentsDir)
		ext := coverExt
		if coverFile == nil {
			ext = notes.CoverExt(coverURL)
		}
		if ext == "" {
			ext = ".jpg"
		}
		name := notes.CoverName(notes.LNVolumeTitle(series, volume), ext)
		dest := longPathServer(filepath.Join(attachAbs, name))
		var err error
		if coverFile != nil {
			err = notes.SaveCoverBytes(dest, coverFile)
		} else if !notes.IsValidURL(coverURL) {
			writeJSON(w, http.StatusBadRequest, errBody("invalid cover url"))
			return
		} else {
			err = notes.DownloadCover(coverURL, dest)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("cover save failed: "+err.Error()))
			return
		}
		coverName = name
	}

	os.Remove(longPathServer(notes.VolumePath(b.Path, series, volume)))
	res, err := notes.SaveLNVolume(opts, series, volume, volumes, "", strings.TrimSpace(link), strings.TrimSpace(releasedEN), description, coverName)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.bustCoverIndex()
	s.bustBackfillStatus()
	s.st.LogEvent("backfill", fmt.Sprintf("Manually filled %s volume %d", series, volume))
	writeJSON(w, http.StatusOK, map[string]any{"status": "filled", "cover": res.Cover, "title": res.Title})
}

// backfillStatusTTL is how long the per-series incomplete/missing counts are
// reused before a rebuild (a full _volumes scan across LN series).
const backfillStatusTTL = 60 * time.Second

// handleBackfillStatus reports, per LN book id, how many of its volumes are
// incomplete or missing — the Books grid badges covers with a non-zero count so
// the user sees which series still want a backfill. Open (read-only).
func (s *Server) handleBackfillStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backfillStatus())
}

func (s *Server) backfillStatus() map[string]int {
	s.bfStatusMu.Lock()
	if s.bfStatus != nil && time.Since(s.bfStatusAt) < backfillStatusTTL {
		m := s.bfStatus
		s.bfStatusMu.Unlock()
		return m
	}
	s.bfStatusMu.Unlock()

	out := map[string]int{}
	if books, err := s.st.ListBooks(); err == nil {
		for _, b := range books {
			if b.Kind != "ln" || strings.TrimSpace(b.Path) == "" || b.Volumes <= 0 {
				continue
			}
			series := strings.TrimSuffix(filepath.Base(b.Path), filepath.Ext(b.Path))
			pending := 0
			for _, st := range notes.VolumeStates(b.Path, series, b.Volumes) {
				if st.State != "resolved" {
					pending++
				}
			}
			if pending > 0 {
				out[strconv.FormatInt(b.ID, 10)] = pending
			}
		}
	}
	s.bfStatusMu.Lock()
	s.bfStatus, s.bfStatusAt = out, time.Now()
	s.bfStatusMu.Unlock()
	return out
}

// bustBackfillStatus forces the Books-grid badge counts to rebuild on next fetch.
func (s *Server) bustBackfillStatus() {
	s.bfStatusMu.Lock()
	s.bfStatus = nil
	s.bfStatusMu.Unlock()
}

// idFromPath reads the {id} path value as an int64 (0 when absent/invalid).
func idFromPath(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}
