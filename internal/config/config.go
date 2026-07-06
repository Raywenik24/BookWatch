package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds runtime settings. Phase 1: env + CLI flags only.
// Later the UI-editable bits move to the SQLite settings table.
type Config struct {
	UserAgent string
	Timeout   time.Duration
	DBPath    string

	VaultDir       string // absolute vault root
	NewNoteDir     string // relative to vault — where new #LightNovel notes go
	AttachmentsDir string // relative to vault — where #LightNovel covers go
	ScanRoot       string // folder scanned for #LightNovel notes

	// Book* mirror the fields above for #Book notes. Left blank by default —
	// the settings/effective lookup falls back to the LN fields above, so an
	// existing single-folder setup keeps working unchanged.
	BookNewNoteDir     string
	BookAttachmentsDir string
	BookScanRoot       string

	// ReadingLogPath points at the unified completed-reads note (`_Read.md`,
	// issue #63). Vault-relative or absolute (via vault.ResolvePath). Blank by
	// default — the reading engine is inert until a path is set in the UI.
	ReadingLogPath string

	Port      string // HTTP listen port
	Password  string // shared password for write endpoints
	CheckCron string // cron expr for the scheduled check
	GBKey     string // Google Books API key — needed for covers; keyless quota is now zero
}

// Default reads env vars, falling back to sane defaults.
func Default() Config {
	loadDotEnv()
	vault := env("BOOKWATCH_VAULT_DIR", "./vault")
	return Config{
		UserAgent:      env("BOOKWATCH_USER_AGENT", "Mozilla/5.0 (page-watcher/1.0)"),
		Timeout:        time.Duration(envInt("BOOKWATCH_TIMEOUT", 30)) * time.Second,
		DBPath:         env("BOOKWATCH_DB_PATH", "bookwatch.db"),
		VaultDir:       vault,
		NewNoteDir:     env("BOOKWATCH_NEW_NOTE_DIR", "LightNovel"),
		AttachmentsDir: env("BOOKWATCH_ATTACHMENTS_DIR", "LightNovel/_attachments"),
		ScanRoot:       env("BOOKWATCH_SCAN_ROOT", vault+"/LightNovel"),

		BookNewNoteDir:     env("BOOKWATCH_BOOK_NEW_NOTE_DIR", ""),
		BookAttachmentsDir: env("BOOKWATCH_BOOK_ATTACHMENTS_DIR", ""),
		BookScanRoot:       env("BOOKWATCH_BOOK_SCAN_ROOT", ""),

		ReadingLogPath: env("BOOKWATCH_READING_LOG_PATH", ""),

		Port:      env("BOOKWATCH_PORT", "8080"),
		Password:  env("BOOKWATCH_PASSWORD", ""),
		CheckCron: env("BOOKWATCH_CHECK_CRON", "0 9 * * *"),
		GBKey:     env("BOOKWATCH_GB_KEY", ""),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

var dotEnvOnce sync.Once

// loadDotEnv reads KEY=VALUE pairs from a gitignored .env in the working dir,
// so the real vault path and password live in one untracked file instead of
// being re-typed each run. Real environment variables (e.g. set by run.ps1 or
// launch.json) always win — .env only fills in what isn't already set.
func loadDotEnv() {
	dotEnvOnce.Do(func() {
		data, err := os.ReadFile(".env")
		if err != nil {
			return // no .env is fine — fall back to env vars + defaults
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			val = strings.Trim(strings.TrimSpace(val), `"'`)
			if key != "" && os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	})
}
