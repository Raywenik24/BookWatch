// Package notes creates new Obsidian notes from a novel URL: scrape, download
// cover, write markdown. Ports createNote.py + build_note_markdown.py, with
// added created/modified frontmatter and a duplicate check.
package notes

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bookwatch/internal/scraper"
)

// ErrDuplicate is returned when a note with the same Link already exists.
var ErrDuplicate = errors.New("a note with this link already exists")

var (
	dlPrefixRE = regexp.MustCompile(`(?i)^Download\s+`)
	epubSufRE  = regexp.MustCompile(`(?i)\s*(Light Novel )?Epub$`)
	badCharRE  = regexp.MustCompile(`[<>"/\\|?*]`)
)

// DupChecker is the slice of the store this package needs (optional).
type DupChecker interface {
	BookExists(link string) (bool, error)
}

// Options are the vault paths for writing.
type Options struct {
	VaultDir       string // absolute vault root
	NewNoteDir     string // relative to vault, where the .md goes
	AttachmentsDir string // relative to vault, where the cover goes
}

// Result describes a created note.
type Result struct {
	Path    string
	Title   string
	Volumes int
	Cover   string // cover attachment filename (no path)
}

// IsValidURL ports note_sanitization.is_valid_url.
func IsValidURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// Sanitize ports note_sanitization.sanitize_filename.
func Sanitize(title string, removeSpaces bool) string {
	title = dlPrefixRE.ReplaceAllString(title, "")
	title = epubSufRE.ReplaceAllString(title, "")
	title = strings.ReplaceAll(title, ":", " -")
	title = badCharRE.ReplaceAllString(title, "")
	title = strings.TrimSpace(title)
	if removeSpaces {
		title = strings.ReplaceAll(title, " ", "")
	}
	return title
}

// BuildNote renders the markdown for a new note (frontmatter + body).
func BuildNote(nd scraper.NovelData, sourceURL, coverName, today string) string {
	title := Sanitize(nd.Title, false)
	return fmt.Sprintf(`---
Series: %s
Link: %s
Volumes: %d
Read Volumes:
Cover: "[[%s]]"
tags:
  - "#LightNovel"
Status:
Series Status:
created: %s
modified: %s
---
### %s

![[%s]]

[[Light Novel]]

%s
`, title, sourceURL, nd.Volumes, coverName, today, today, title, coverName, nd.Description)
}

// Create scrapes the URL and writes a new note + cover into the vault.
// dup may be nil to skip the duplicate check. rl are the scrape rules.
func Create(o Options, sc *scraper.Client, dup DupChecker, rl scraper.Rules, sourceURL string) (Result, error) {
	if !IsValidURL(sourceURL) {
		return Result{}, errors.New("invalid URL")
	}
	if dup != nil {
		exists, err := dup.BookExists(sourceURL)
		if err != nil {
			return Result{}, err
		}
		if exists {
			return Result{}, ErrDuplicate
		}
	}

	nd, err := sc.NovelData(sourceURL, rl)
	if err != nil {
		return Result{}, err
	}
	title := Sanitize(nd.Title, false)
	today := time.Now().Format("2006-01-02")

	// Download cover.
	coverName := "cover_" + Sanitize(nd.Title, true) + coverExt(nd.CoverURL)
	attachAbs := filepath.Join(o.VaultDir, filepath.FromSlash(o.AttachmentsDir))
	if err := os.MkdirAll(attachAbs, 0o755); err != nil {
		return Result{}, err
	}
	if err := download(nd.CoverURL, filepath.Join(attachAbs, coverName)); err != nil {
		return Result{}, fmt.Errorf("cover download: %w", err)
	}

	// Write note.
	noteAbs := filepath.Join(o.VaultDir, filepath.FromSlash(o.NewNoteDir))
	if err := os.MkdirAll(noteAbs, 0o755); err != nil {
		return Result{}, err
	}
	mdPath := filepath.Join(noteAbs, title+".md")
	content := BuildNote(nd, sourceURL, coverName, today)
	if err := os.WriteFile(mdPath, []byte(content), 0o644); err != nil {
		return Result{}, err
	}

	return Result{Path: mdPath, Title: title, Volumes: nd.Volumes, Cover: coverName}, nil
}

func coverExt(coverURL string) string {
	if u, err := url.Parse(coverURL); err == nil {
		if ext := strings.ToLower(path.Ext(u.Path)); ext != "" && len(ext) <= 5 {
			return ext
		}
	}
	return ".jpg"
}

func download(url, dest string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
