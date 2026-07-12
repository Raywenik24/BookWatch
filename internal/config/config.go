package config

import (
	"fmt"
	"os"
	"path/filepath"
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

	// CalibreLibraryPath is the Calibre library folder (the one holding
	// metadata.db) the import reads (#75). ImportStagingDir is where staged
	// import notes are written — vault-relative or absolute, and deliberately
	// outside the scan roots so review notes aren't picked up as tracked. Both
	// are settings-table editable; these are the CLI/env fallbacks.
	CalibreLibraryPath string
	ImportStagingDir   string

	// ImportFilterField / ImportFilterValues optionally restrict the import to
	// books carrying a particular Calibre identifier (e.g. field "czyj", values
	// "andrzej"). Blank field = import everything. Values is a comma/newline list;
	// import_filter_include_missing (settings-only) also admits books lacking the
	// field. Settings-table editable; these are the CLI/env fallbacks.
	ImportFilterField  string
	ImportFilterValues string

	Port      string // HTTP listen port
	Password  string // shared password for write endpoints
	CheckCron string // cron expr for the scheduled check
	GBKey     string // Google Books API key — needed for covers; keyless quota is now zero
}

// Default reads env vars, falling back to sane defaults.
func Default() Config {
	loadDotEnv()
	vault := env("BOOKWATCH_VAULT_DIR", "./vault")

	dbPath := os.Getenv("BOOKWATCH_DB_PATH")
	if dbPath == "" {
		dbPath = defaultDBPath()
		migrateLegacyDB(dbPath)
	}

	return Config{
		UserAgent:      env("BOOKWATCH_USER_AGENT", "Mozilla/5.0 (page-watcher/1.0)"),
		Timeout:        time.Duration(envInt("BOOKWATCH_TIMEOUT", 30)) * time.Second,
		DBPath:         dbPath,
		VaultDir:       vault,
		NewNoteDir:     env("BOOKWATCH_NEW_NOTE_DIR", "LightNovel"),
		AttachmentsDir: env("BOOKWATCH_ATTACHMENTS_DIR", "LightNovel/_attachments"),
		ScanRoot:       env("BOOKWATCH_SCAN_ROOT", vault+"/LightNovel"),

		BookNewNoteDir:     env("BOOKWATCH_BOOK_NEW_NOTE_DIR", ""),
		BookAttachmentsDir: env("BOOKWATCH_BOOK_ATTACHMENTS_DIR", ""),
		BookScanRoot:       env("BOOKWATCH_BOOK_SCAN_ROOT", ""),

		ReadingLogPath: env("BOOKWATCH_READING_LOG_PATH", ""),

		CalibreLibraryPath: env("BOOKWATCH_CALIBRE_LIBRARY_PATH", ""),
		ImportStagingDir:   env("BOOKWATCH_IMPORT_STAGING_DIR", "_CalibreImport"),
		ImportFilterField:  env("BOOKWATCH_IMPORT_FILTER_FIELD", ""),
		ImportFilterValues: env("BOOKWATCH_IMPORT_FILTER_VALUES", ""),

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

// defaultDBPath returns config/bookwatch.db resolved next to the executable
// (not the working dir), so a portable exe finds its db regardless of the
// dir it was launched from. Falls back to a cwd-relative path if the
// executable can't be resolved.
func defaultDBPath() string {
	base := "."
	if exe, err := os.Executable(); err == nil {
		base = filepath.Dir(exe)
	}
	return filepath.Join(base, "config", "bookwatch.db")
}

// migrateLegacyDB moves a pre-#79 root-level bookwatch.db (plus its -wal/-shm
// siblings) into the new config/ dir, one time, on startup. No-op if the new
// path already has a db (already migrated) or no legacy db exists.
func migrateLegacyDB(newPath string) {
	if _, err := os.Stat(newPath); err == nil {
		return
	}

	dir := filepath.Dir(newPath)
	legacy := filepath.Join(filepath.Dir(dir), "bookwatch.db")
	if _, err := os.Stat(legacy); err != nil {
		return
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not create config dir for db migration:", err)
		return
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := legacy + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, newPath+suffix); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not migrate", src, "to config/:", err)
		}
	}
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
