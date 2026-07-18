package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"bookwatch/internal/importer"
	"bookwatch/internal/notes"
	"bookwatch/internal/provider"
	"bookwatch/internal/service"
	"bookwatch/internal/store"
	"bookwatch/internal/vault"
)

// In-app import review (#77 follow-up). An alternative to reviewing the staged
// notes in Obsidian: step through them one at a time in the browser, pick a
// candidate link for an unmatched note (which also pulls the source blurb), edit
// the result (title/description/status/link/cover), then accept — which moves
// just that note out of staging into its real folder — or reject it. A
// bulk-accept moves every clean (matched, non-duplicate) note at once. All of
// this works off the *latest* session, so it's available after a fully-completed
// run (whose session is 'done', not 'resumable').

// ── shared helpers ─────────────────────────────────────────────

// finalizeDest builds the routed destination dirs for finalize/accept, plus the
// LN new-note relative path (for the Excluded-files hint).
func (s *Server) finalizeDest() (importer.FinalizeDest, string) {
	vaultDir := s.effective("vault_dir", s.cfg.VaultDir)
	lnNoteRel := s.effective("new_note_dir", s.cfg.NewNoteDir)
	return importer.FinalizeDest{
		NoteDirLN:     vault.ResolvePath(vaultDir, lnNoteRel),
		AttachDirLN:   vault.ResolvePath(vaultDir, s.effective("attachments_dir", s.cfg.AttachmentsDir)),
		NoteDirBook:   vault.ResolvePath(vaultDir, s.effectiveBookNewNoteDir()),
		AttachDirBook: vault.ResolvePath(vaultDir, s.effectiveBookAttachmentsDir()),
	}, lnNoteRel
}

// parseStagedFiles unmarshals an item's staged_files JSON array.
func parseStagedFiles(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// primaryNote is the item's main .md (the series or book note — first in the
// staged-files list); noteFilesOf is every .md (primary + LN volume notes).
func primaryNote(files []string) string {
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f), ".md") {
			return f
		}
	}
	return ""
}

func noteFilesOf(files []string) []string {
	var out []string
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f), ".md") {
			out = append(out, f)
		}
	}
	return out
}

func parseCandidates(raw string) []importer.Candidate {
	out := []importer.Candidate{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

// reviewItem loads one review item and its primary staged note path, writing the
// appropriate error response and returning ok=false on any problem. An item whose
// staged note has already been moved/deleted (accepted or rejected in another tab,
// or stale client state after back-navigating) is reported as "stale" rather than
// a hard error, so the reviewer can skip it instead of showing a file-not-found.
func (s *Server) reviewItem(w http.ResponseWriter, id int64) (store.ImportItem, string, bool) {
	it, ok, err := s.st.GetImportItem(id)
	if err != nil {
		writeErr(w, err)
		return it, "", false
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("no such review item"))
		return it, "", false
	}
	np := primaryNote(parseStagedFiles(it.StagedFiles))
	if np == "" {
		writeJSON(w, http.StatusBadRequest, errBody("this item has no staged note (nothing to review)"))
		return it, "", false
	}
	if _, err := os.Stat(np); err != nil {
		writeJSON(w, http.StatusGone, map[string]any{"error": "already handled", "stale": true})
		return it, "", false
	}
	return it, np, true
}

// reviewDetail re-reads an item's staged note and writes the full editable
// payload (frontmatter fields + inlined cover + candidates + import flags).
func (s *Server) reviewDetail(w http.ResponseWriter, it store.ImportItem, notePath string, extra ...map[string]any) {
	n, err := vault.ReadNote(notePath)
	if err != nil {
		writeErr(w, err)
		return
	}
	b, _ := json.Marshal(n)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	m["id"] = it.ID
	m["import_kind"] = it.Kind
	m["unmatched"] = it.State == "unmatched"
	m["duplicate_of"] = it.DuplicateOf
	m["candidates"] = parseCandidates(it.Candidates)
	if n.Cover != "" {
		m["cover_data"] = fileCoverDataURI(filepath.Join(filepath.Dir(notePath), n.Cover))
	}
	for _, e := range extra {
		for k, v := range e {
			m[k] = v
		}
	}
	writeJSON(w, http.StatusOK, m)
}

// fileCoverDataURI reads a local cover image into a data: URI (best-effort — a
// missing/unreadable file yields ""), so the reviewer shows the staged cover
// without a separate route (the staged cover lives beside its note, outside the
// attachments dir the cover endpoint serves).
func fileCoverDataURI(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	mime := "image/jpeg"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func idFrom(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	return id
}

// reviewableCount is how many of a session's items still have their staged note
// on disk (not yet accepted or rejected) — what keeps a finished import in the
// 'review' state until the queue is emptied.
func (s *Server) reviewableCount(items []store.ImportItem) int {
	n := 0
	for _, it := range items {
		if s.isReviewable(it) {
			n++
		}
	}
	return n
}

// isReviewable reports whether an item's staged note is still on disk — i.e. not
// yet accepted (moved) or rejected (deleted). Accept/reject only touch the file,
// not the item's persisted state, so this on-disk check is the single source of
// truth for "still needs a decision" (#85: status and reviewer must agree).
func (s *Server) isReviewable(it store.ImportItem) bool {
	np := primaryNote(parseStagedFiles(it.StagedFiles))
	if np == "" {
		return false
	}
	_, err := os.Stat(np)
	return err == nil
}

// pendingReviewCounts is the unmatched/duplicate counts among still-reviewable
// items — the same figures the reviewer queue's filter checkboxes show, used so
// the status panel's "needs a decision" summary never disagrees with what
// opening the reviewer actually reveals.
func (s *Server) pendingReviewCounts(items []store.ImportItem) (unmatched, duplicates int) {
	for _, it := range items {
		if !s.isReviewable(it) {
			continue
		}
		if it.State == "unmatched" {
			unmatched++
		}
		if it.DuplicateOf != "" {
			duplicates++
		}
	}
	return
}

// ── list ───────────────────────────────────────────────────────

// handleReviewList returns the review queue: every latest-session item whose
// staged note is still on disk (i.e. not yet accepted or rejected), with the
// flags the UI filters on (unmatched / duplicate / clean) plus its candidates.
func (s *Server) handleReviewList(w http.ResponseWriter, r *http.Request) {
	sess, ok, err := s.st.LatestImportSession()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "counts": map[string]int{}})
		return
	}
	items, _ := s.st.ListImportItems(sess.ID)
	out := make([]map[string]any, 0, len(items))
	var nUn, nDup, nClean int
	for _, it := range items {
		if !s.isReviewable(it) {
			continue // already accepted/rejected — gone from staging
		}
		unmatched := it.State == "unmatched"
		dup := it.DuplicateOf != ""
		clean := it.State == "matched" && !dup
		switch {
		case unmatched:
			nUn++
		case dup:
			nDup++
		case clean:
			nClean++
		}
		out = append(out, map[string]any{
			"id": it.ID, "import_kind": it.Kind, "title": it.Title,
			"state": it.State, "unmatched": unmatched, "duplicate_of": it.DuplicateOf,
			"clean": clean, "resolved_link": it.ResolvedLink,
			"candidates": parseCandidates(it.Candidates),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       out,
		"staging_dir": sess.StagingDir,
		"counts":      map[string]int{"unmatched": nUn, "duplicates": nDup, "clean": nClean, "total": len(out)},
	})
}

// handleReviewItem returns one item's full editable detail.
func (s *Server) handleReviewItem(w http.ResponseWriter, r *http.Request) {
	it, np, ok := s.reviewItem(w, idFrom(r))
	if !ok {
		return
	}
	s.reviewDetail(w, it, np)
}

// ── pick a candidate ───────────────────────────────────────────

// handleReviewPick resolves an unmatched note to a chosen candidate URL: it sets
// Link, drops the #import/unmatched tag + the candidate-links block from the
// body, pulls the source's blurb when the note has none, and flips the item to
// matched.
func (s *Server) handleReviewPick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	it, np, ok := s.reviewItem(w, body.ID)
	if !ok {
		return
	}
	if err := s.resolveStagedLink(&it, np, body.URL); err != nil {
		writeErr(w, err)
		return
	}
	s.reviewDetail(w, it, np)
}

// resolveStagedLink turns an unmatched staged note into a matched one against a
// real catalog URL — whether it arrived via "Use this" (a candidate) or a manual
// paste into the Link field. It sets Link, derives + writes Work ID from the URL,
// drops the #import/unmatched tag + the candidate-links block, pulls the source
// blurb when the note has none, and flips the item to matched. The passed item is
// updated in place. A synthetic/blank URL is a no-op resolve (just the link set).
func (s *Server) resolveStagedLink(it *store.ImportItem, notePath, url string) error {
	if err := vault.UpdateLink(notePath, url); err != nil {
		return err
	}
	if !isResolvedLink(url) {
		// Not a real catalog link (blank or the synthetic placeholder) — just
		// record it, leave the unmatched flag as-is.
		it.ResolvedLink = url
		return s.st.SetImportItemStaged(it.ID, it.StagedFiles, url, it.State, it.DuplicateOf)
	}
	n, err := vault.ReadNote(notePath)
	if err != nil {
		return err
	}
	// Drop the #import/unmatched tag now that it's resolved.
	var tags []string
	for _, t := range n.Tags {
		if strings.EqualFold(strings.TrimPrefix(t, "#"), "import/unmatched") {
			continue
		}
		tags = append(tags, t)
	}
	if err := vault.UpdateTags(notePath, tags); err != nil {
		return err
	}
	if wid := workIDFromURL(url); wid != "" && strings.TrimSpace(n.WorkID) == "" {
		_ = vault.UpdateWorkID(notePath, wid)
	}
	stripCandidateBlock(notePath)
	// Always refresh the description from the newly chosen source — picking a
	// different candidate should replace a stale blurb, not just fill a blank one.
	if d := s.sourceDescription(url); d != "" {
		_ = vault.UpdateDescription(notePath, d)
	}
	it.State, it.ResolvedLink = "matched", url
	return s.st.SetImportItemStaged(it.ID, it.StagedFiles, url, "matched", it.DuplicateOf)
}

// isResolvedLink reports whether a URL is a real catalog link rather than blank
// or the synthetic `unmatched.bookwatch.invalid` placeholder.
func isResolvedLink(url string) bool {
	url = strings.TrimSpace(url)
	return url != "" && !strings.Contains(url, "unmatched.bookwatch.invalid")
}

// workIDFromURL derives the catalog id a note's Work ID should carry: the numeric
// Lubimyczytać book id from `/ksiazka/<id>`, or the OpenLibrary work id from
// `/works/<id>`. Empty when the host isn't recognized.
func workIDFromURL(url string) string {
	if m := lcBookIDRE.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	if i := strings.Index(url, "/works/"); i >= 0 {
		id := url[i+len("/works/"):]
		if j := strings.IndexAny(id, "/?#"); j >= 0 {
			id = id[:j]
		}
		return strings.TrimSpace(id)
	}
	return ""
}

var lcBookIDRE = regexp.MustCompile(`/ksiazka/(\d+)`)

// handleReviewPull fetches the description and/or cover for a staged note from a
// source URL (the note's link, or one the reviewer just pasted) and applies it —
// the explicit "Load description" / "Load cover" buttons for when a manual link
// replaces the wrong candidates. `what` is "description", "cover", or "both".
func (s *Server) handleReviewPull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   int64  `json:"id"`
		URL  string `json:"url"`
		What string `json:"what"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	it, np, ok := s.reviewItem(w, body.ID)
	if !ok {
		return
	}
	url := strings.TrimSpace(body.URL)
	if url == "" {
		url = it.ResolvedLink
	}
	if !isResolvedLink(url) {
		writeJSON(w, http.StatusBadRequest, errBody("paste a real source link first"))
		return
	}
	// Persist the (possibly just-pasted) link up front so it survives the
	// reviewDetail re-render below even if nothing is actually found at it.
	if url != it.ResolvedLink {
		if err := vault.UpdateLink(np, url); err == nil {
			it.ResolvedLink = url
			_ = s.st.SetImportItemStaged(it.ID, it.StagedFiles, url, it.State, it.DuplicateOf)
		}
	}
	wantDesc := body.What == "description" || body.What == "both"
	wantCover := body.What == "cover" || body.What == "both"
	descFound, coverFound := false, false

	if wantDesc {
		if d := s.sourceDescription(url); d != "" {
			_ = vault.UpdateDescription(np, d)
			descFound = true
		}
	}
	if wantCover {
		if cu := s.sourceCoverURL(url); cu != "" {
			n, _ := vault.ReadNote(np)
			dir := filepath.Dir(np)
			ext := notes.CoverExt(cu)
			if ext == "" {
				ext = ".jpg"
			}
			coverName := notes.CoverName(n.Title, ext)
			newCoverPath := filepath.Join(dir, coverName)
			if err := notes.DownloadCover(cu, newCoverPath); err == nil {
				coverFound = true
				if n.Cover != "" && n.Cover != coverName {
					os.Remove(filepath.Join(dir, n.Cover))
				}
				_ = vault.UpdateCover(np, coverName)
				files := parseStagedFiles(it.StagedFiles)
				if n.Cover != "" {
					replaceInSlice(files, filepath.Join(dir, n.Cover), newCoverPath)
				} else {
					files = append(files, newCoverPath)
				}
				raw, _ := json.Marshal(files)
				s.st.SetImportItemStaged(it.ID, string(raw), it.ResolvedLink, it.State, it.DuplicateOf)
				it.StagedFiles = string(raw)
			}
		}
	}
	// Report what was actually fetched — the reviewer must not infer success by
	// diffing before/after text, which false-negatives when the fetched value
	// happens to match what was already there.
	found := (wantDesc && descFound) || (wantCover && coverFound)
	s.reviewDetail(w, it, np, map[string]any{"pull_found": found})
}

// sourceCoverURL returns a book's cover image URL from a resolved link —
// Lubimyczytać book pages carry one directly; an OpenLibrary work resolves to
// its own cover (via WorkByID), falling back to the first edition cover when
// the work record has none. "" when nothing is found.
func (s *Server) sourceCoverURL(url string) string {
	if strings.Contains(url, "lubimyczytac.pl") && s.lc != nil {
		if b, err := s.lc.BookDetail(url); err == nil {
			return b.CoverURL
		}
		return ""
	}
	if i := strings.Index(url, "/works/"); i >= 0 && s.ol != nil {
		id := strings.Trim(url[i+len("/works/"):], "/")
		if id == "" {
			return ""
		}
		if c, err := s.ol.WorkByID(id); err == nil && c.CoverURL != "" {
			return c.CoverURL
		}
		if eds, err := s.ol.WorkEditions(id); err == nil {
			for _, e := range eds {
				if e.CoverURL != "" {
					return e.CoverURL
				}
			}
		}
	}
	return ""
}

// sourceDescription fetches a book's blurb from a resolved candidate URL —
// Lubimyczytać book pages via BookDetail, OpenLibrary work URLs via WorkDetail.
// Best-effort: an unrecognized host or a fetch failure returns "".
func (s *Server) sourceDescription(url string) string {
	if strings.Contains(url, "lubimyczytac.pl") {
		if s.lc != nil {
			if b, err := s.lc.BookDetail(url); err == nil {
				return b.Description
			}
		}
		return ""
	}
	if i := strings.Index(url, "/works/"); i >= 0 {
		id := strings.Trim(url[i+len("/works/"):], "/")
		if o, ok := s.ol.(interface {
			WorkDetail(string) (provider.Work, error)
		}); ok && id != "" {
			if wd, err := o.WorkDetail(id); err == nil {
				return wd.Description
			}
		}
	}
	return ""
}

// stripCandidateBlock removes the "**Unmatched — candidate links to review:**"
// section (and everything after it) from a staged note's body — the review aid
// is meaningless once a candidate has been chosen.
func stripCandidateBlock(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s := string(data)
	if i := strings.Index(s, "**Unmatched — candidate links to review:**"); i >= 0 {
		out := strings.TrimRight(s[:i], "\n ") + "\n"
		_ = vault.AtomicWrite(path, []byte(out), 0o644)
	}
}

// ── edit ───────────────────────────────────────────────────────

// handleReviewEdit applies edit fields (any subset of status/description/link/
// title) to a staged note. A title change renames the note + its beside-note
// cover and updates the item's staged-files record so accept still finds them.
func (s *Server) handleReviewEdit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          int64   `json:"id"`
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Status      *string `json:"status"`
		Link        *string `json:"link"`
		Author      *string `json:"author"`
		Series      *string `json:"series"`
		SeriesIndex *string `json:"series_index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	it, np, ok := s.reviewItem(w, body.ID)
	if !ok {
		return
	}
	if body.Status != nil {
		if err := vault.UpdateStatus(np, *body.Status); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Description != nil {
		if err := vault.UpdateDescription(np, *body.Description); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Author != nil {
		if err := vault.UpdateAuthor(np, *body.Author); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Series != nil {
		if err := vault.UpdateSeries(np, *body.Series); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.SeriesIndex != nil {
		if err := vault.UpdateSeriesIndex(np, *body.SeriesIndex); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Link != nil && strings.TrimSpace(*body.Link) != it.ResolvedLink {
		// A manually pasted (or edited) link resolves the note the same way "Use
		// this" does — drop the unmatched tag, fill Work ID, etc.
		if err := s.resolveStagedLink(&it, np, strings.TrimSpace(*body.Link)); err != nil {
			writeErr(w, err)
			return
		}
	}
	if body.Title != nil && strings.TrimSpace(*body.Title) != "" {
		newPath, err := s.renameStagedNote(&it, np, *body.Title)
		if err != nil {
			code := http.StatusBadRequest
			if strings.Contains(err.Error(), "already exists") {
				code = http.StatusConflict
			}
			writeJSON(w, code, errBody(err.Error()))
			return
		}
		np = newPath
	}
	s.reviewDetail(w, it, np)
}

// renameStagedNote renames a staged note (and its beside-note cover) in place,
// updating the in-body heading, Title:/Cover: fields, and the item's staged-files
// record. Staged covers live next to the note (not in the attachments dir), so
// this can't reuse notes.RenameNote.
func (s *Server) renameStagedNote(it *store.ImportItem, oldPath, newTitle string) (string, error) {
	dir := filepath.Dir(oldPath)
	newBase := notes.Sanitize(newTitle, false)
	if newBase == "" {
		return "", fmt.Errorf("empty title")
	}
	newPath := filepath.Join(dir, newBase+".md")
	if !strings.EqualFold(newPath, oldPath) {
		if _, err := os.Stat(newPath); err == nil {
			return "", fmt.Errorf("a staged note named %s.md already exists", newBase)
		}
	}

	n, _ := vault.ReadNote(oldPath)
	oldCover := n.Cover
	newCover := oldCover
	files := parseStagedFiles(it.StagedFiles)

	if oldCover != "" {
		newCover = notes.CoverName(newTitle, filepath.Ext(oldCover))
		oldCoverPath := filepath.Join(dir, oldCover)
		newCoverPath := filepath.Join(dir, newCover)
		if newCover != oldCover {
			if _, err := os.Stat(oldCoverPath); err == nil {
				if err := vault.RenameWithRetry(oldCoverPath, newCoverPath); err != nil {
					return "", err
				}
				replaceInSlice(files, oldCoverPath, newCoverPath)
			} else {
				newCover = oldCover
			}
		}
	}
	if !strings.EqualFold(newPath, oldPath) {
		if err := vault.RenameWithRetry(oldPath, newPath); err != nil {
			return "", err
		}
		replaceInSlice(files, oldPath, newPath)
	}
	_ = vault.SetTitleHeading(newPath, newBase)
	_ = vault.UpdateTitleField(newPath, newBase)
	if newCover != oldCover {
		_ = vault.UpdateCover(newPath, newCover)
	}

	raw, _ := json.Marshal(files)
	it.Title = newBase
	if err := s.st.SetImportItemStaged(it.ID, string(raw), it.ResolvedLink, it.State, it.DuplicateOf); err != nil {
		return "", err
	}
	it.StagedFiles = string(raw)
	return newPath, nil
}

func replaceInSlice(ss []string, old, new string) {
	for i, v := range ss {
		if v == old {
			ss[i] = new
		}
	}
}

// ── cover ──────────────────────────────────────────────────────

// handleReviewCover replaces a staged note's cover from an uploaded image
// (multipart) or a pasted URL, saving it beside the note (where staged covers
// live) and rewriting the Cover field + embed.
func (s *Server) handleReviewCover(w http.ResponseWriter, r *http.Request) {
	it, np, ok := s.reviewItem(w, idFrom(r))
	if !ok {
		return
	}
	n, err := vault.ReadNote(np)
	if err != nil {
		writeErr(w, err)
		return
	}
	dir := filepath.Dir(np)

	var ext string
	var save func(dest string) error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
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

	coverName := notes.CoverName(n.Title, ext)
	newCoverPath := filepath.Join(dir, coverName)
	if err := save(newCoverPath); err != nil {
		writeErr(w, err)
		return
	}
	if n.Cover != "" && n.Cover != coverName {
		os.Remove(filepath.Join(dir, n.Cover))
	}
	if err := vault.UpdateCover(np, coverName); err != nil {
		writeErr(w, err)
		return
	}
	// Keep the staged-files record pointing at the current cover.
	files := parseStagedFiles(it.StagedFiles)
	if n.Cover != "" {
		replaceInSlice(files, filepath.Join(dir, n.Cover), newCoverPath)
	} else {
		files = append(files, newCoverPath)
	}
	raw, _ := json.Marshal(files)
	s.st.SetImportItemStaged(it.ID, string(raw), it.ResolvedLink, it.State, it.DuplicateOf)
	it.StagedFiles = string(raw)
	s.reviewDetail(w, it, np)
}

// ── accept / reject / bulk ─────────────────────────────────────

// handleReviewAccept moves one reviewed item's note(s) + cover(s) out of staging
// into their real destination (collision-safe), so the reviewer keeps it. The
// moved notes get tracked on the next vault refresh (the reviewer triggers one
// when it closes).
func (s *Server) handleReviewAccept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	it, ok, err := s.st.GetImportItem(body.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("no such review item"))
		return
	}
	dest, _ := s.finalizeDest()
	res, err := importer.FinalizeNotes(s.effectiveImportStagingDir(), noteFilesOf(parseStagedFiles(it.StagedFiles)), dest)
	if err != nil {
		writeErr(w, err)
		return
	}
	_ = importer.TrimResolvedItem(s.effectiveImportStagingDir(), it.Title)
	writeJSON(w, http.StatusOK, map[string]any{"notes": res.Notes, "covers": res.Covers, "skipped": res.Skipped})
}

// handleReviewReject deletes one item's staged files without moving them — the
// reviewer doesn't want this note. The item's uuid stays recorded, so a later
// re-run won't re-propose the rejected book.
func (s *Server) handleReviewReject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	it, ok, err := s.st.GetImportItem(body.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("no such review item"))
		return
	}
	for _, f := range parseStagedFiles(it.StagedFiles) {
		os.Remove(f)
	}
	_ = importer.TrimResolvedItem(s.effectiveImportStagingDir(), it.Title)
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// handleReviewAcceptClean moves every remaining clean (matched, non-duplicate,
// non-unmatched) item out of staging in one pass, then refreshes the vault so
// they're all tracked. The unmatched + duplicate notes stay in staging for the
// reviewer to handle by hand.
func (s *Server) handleReviewAcceptClean(w http.ResponseWriter, r *http.Request) {
	sess, ok, err := s.st.LatestImportSession()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"notes": 0, "covers": 0})
		return
	}
	items, _ := s.st.ListImportItems(sess.ID)
	var files []string
	var titles []string
	for _, it := range items {
		if it.State == "matched" && it.DuplicateOf == "" {
			files = append(files, noteFilesOf(parseStagedFiles(it.StagedFiles))...)
			titles = append(titles, it.Title)
		}
	}
	dest, _ := s.finalizeDest()
	res, err := importer.FinalizeNotes(s.effectiveImportStagingDir(), files, dest)
	if err != nil {
		writeErr(w, err)
		return
	}
	_ = importer.TrimResolvedItems(s.effectiveImportStagingDir(), titles)
	refresh, _ := service.RefreshVault(s.st, ScanRoots(s.cfg, s.st))
	s.st.LogEvent("import", "Accepted clean import notes: "+strconv.Itoa(res.Notes)+" moved")
	s.publishImportStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"notes": res.Notes, "covers": res.Covers, "skipped": res.Skipped, "refresh": refresh,
	})
}
