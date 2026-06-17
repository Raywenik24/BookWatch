package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds runtime settings. Phase 1: env + CLI flags only.
// Later the UI-editable bits move to the SQLite settings table.
type Config struct {
	UserAgent string
	Timeout   time.Duration
	DBPath    string

	VaultDir       string // absolute vault root
	NewNoteDir     string // relative to vault — where new notes go
	AttachmentsDir string // relative to vault — where covers go
	ScanRoot       string // folder scanned for #LightNovel notes

	Port     string // HTTP listen port
	Password string // shared password for write endpoints
	CheckCron string // cron expr for the scheduled check
}

// Default reads env vars, falling back to sane defaults.
func Default() Config {
	vault := env("BOOKWATCH_VAULT_DIR", `./vault`)
	return Config{
		UserAgent:      env("BOOKWATCH_USER_AGENT", "Mozilla/5.0 (page-watcher/1.0)"),
		Timeout:        time.Duration(envInt("BOOKWATCH_TIMEOUT", 30)) * time.Second,
		DBPath:         env("BOOKWATCH_DB_PATH", "bookwatch.db"),
		VaultDir:       vault,
		NewNoteDir:     env("BOOKWATCH_NEW_NOTE_DIR", "LightNovel"),
		AttachmentsDir: env("BOOKWATCH_ATTACHMENTS_DIR", "LightNovel/_attachments"),
		ScanRoot:       env("BOOKWATCH_SCAN_ROOT", vault+"/LightNovel"),
		Port:           env("BOOKWATCH_PORT", "8080"),
		Password:       env("BOOKWATCH_PASSWORD", ""),
		CheckCron:      env("BOOKWATCH_CHECK_CRON", "0 9 * * *"),
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
