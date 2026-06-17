// Package server exposes the HTTP API + embedded web UI. Viewing is open;
// write endpoints require the shared password (see auth).
package server

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"pagewatch/internal/config"
	"pagewatch/internal/notes"
	"pagewatch/internal/scheduler"
	"pagewatch/internal/scraper"
	"pagewatch/internal/sources"
	"pagewatch/internal/store"
)

//go:embed web/index.html
var webFS embed.FS

type Server struct {
	cfg   config.Config
	st    *store.Store
	sc    *scraper.Client
	sched *scheduler.Scheduler
}

func New(cfg config.Config, st *store.Store, sc *scraper.Client, sched *scheduler.Scheduler) *Server {
	return &Server{cfg: cfg, st: st, sc: sc, sched: sched}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/books", s.handleBooks)
	mux.HandleFunc("GET /api/updates", s.handleUpdates)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/sources", s.handleSources)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)

	mux.HandleFunc("POST /api/check", s.auth(s.handleCheck))
	mux.HandleFunc("POST /api/books", s.auth(s.handleAddBook))
	mux.HandleFunc("POST /api/sources", s.auth(s.handleUpsertSource))
	mux.HandleFunc("DELETE /api/sources/{id}", s.auth(s.handleDeleteSource))
	mux.HandleFunc("PUT /api/sources/{id}/rules", s.auth(s.handleSetRules))
	mux.HandleFunc("POST /api/sources/test", s.auth(s.handleTest))
	mux.HandleFunc("PUT /api/settings", s.auth(s.handleSetSettings))
	return logging(mux)
}

// ── middleware ────────────────────────────────────────────────

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-PageWatch-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token != s.cfg.Password {
			writeJSON(w, http.StatusUnauthorized, errBody("unauthorized"))
			return
		}
		h(w, r)
	}
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
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	v, err := s.st.ListRuns(50)
	respond(w, v, err)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"busy": s.sched.Busy()})
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
			"scan_root":       s.effective("scan_root", s.cfg.ScanRoot),
			"new_note_dir":    s.effective("new_note_dir", s.cfg.NewNoteDir),
			"attachments_dir": s.effective("attachments_dir", s.cfg.AttachmentsDir),
		},
		"overrides": saved,
	})
}

// ── write handlers ────────────────────────────────────────────

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !s.sched.Trigger("api") {
		writeJSON(w, http.StatusConflict, errBody("a check is already running"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleAddBook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing url"))
		return
	}
	opts := notes.Options{
		VaultDir:       s.effective("vault_dir", s.cfg.VaultDir),
		NewNoteDir:     s.effective("new_note_dir", s.cfg.NewNoteDir),
		AttachmentsDir: s.effective("attachments_dir", s.cfg.AttachmentsDir),
	}
	rl := sources.NewResolver(s.st).For(body.URL)
	res, err := notes.Create(opts, s.sc, s.st, rl, body.URL)
	if err != nil {
		code := http.StatusBadRequest
		if err == notes.ErrDuplicate {
			code = http.StatusConflict
		}
		writeJSON(w, code, errBody(err.Error()))
		return
	}
	if _, err := s.st.UpsertBook(res.Title, body.URL, res.Path, res.Volumes); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"title": res.Title, "volumes": res.Volumes, "path": res.Path,
	})
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

func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var kv map[string]string
	if err := json.NewDecoder(r.Body).Decode(&kv); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad body"))
		return
	}
	for k, v := range kv {
		if err := s.st.SetSetting(k, v); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// ── helpers ───────────────────────────────────────────────────

// effective returns a settings-table override for key, else the cfg fallback.
func (s *Server) effective(key, fallback string) string {
	if v, ok, _ := s.st.GetSetting(key); ok && v != "" {
		return v
	}
	return fallback
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
