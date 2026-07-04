// Package notes creates new Obsidian notes from a novel URL: scrape, download
// cover, write markdown. Ports createNote.py + build_note_markdown.py, with
// added created/modified frontmatter and a duplicate check.
package notes

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bookwatch/internal/scraper"
	"bookwatch/internal/vault"
)

// ErrDuplicate is returned when a note with the same Link already exists.
var ErrDuplicate = errors.New("a note with this link already exists")

// ErrNoteExists is returned when a note file with the target name already
// exists on disk. Two different links can sanitize to the same filename, and
// the link-based duplicate check wouldn't catch that — so refuse rather than
// silently overwrite a note the user may have edited.
var ErrNoteExists = errors.New("a note with this filename already exists")

// maxCoverBytes caps a downloaded cover image so a runaway response can't fill
// the disk. Cover art is well under this.
const maxCoverBytes = 16 << 20 // 16 MiB

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

// BuildNote renders the markdown for a new LN note (frontmatter + body).
func BuildNote(nd scraper.NovelData, sourceURL, coverName, today string) string {
	title := Sanitize(nd.Title, false)
	return fmt.Sprintf(`---
Series: %s
Author:
Link: %s
Volumes: %d
Read Volumes:
Cover: "[[%s]]"
tags:
  - "#LightNovel"
Status:
  - Backlog
Template_used: LightNovelTemplate
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

// BuildBookNote renders a new book note: frontmatter, the cover embed (when
// there is one), and the OL description (when there is one).
func BuildBookNote(title, author, link, workID, coverName, status, releasedEN, description, today string) string {
	coverField := ""
	coverEmbed := ""
	if coverName != "" {
		coverField = fmt.Sprintf(`"[[%s]]"`, coverName)
		coverEmbed = fmt.Sprintf("![[%s]]\n\n", coverName)
	}
	if status == "" {
		status = "Backlog"
	}
	return fmt.Sprintf(`---
Title: %s
Author: %s
Link: %s
Work ID: %s
Cover: %s
Released EN: %s
Status:
  - %s
tags:
  - "#Book"
Template_used: BookTemplate
created: %s
modified: %s
---
### %s

%s%s
`, title, author, link, workID, coverField, releasedEN, status, today, today, title, coverEmbed, description)
}

// CreateBook writes a new #Book note from catalog data (OpenLibrary) — no
// scraping involved, unlike Create. coverURL/description may be empty. dup
// may be nil to skip the duplicate check.
func CreateBook(o Options, dup DupChecker, title, author, link, workID, coverURL, status, description string) (Result, error) {
	if dup != nil {
		exists, err := dup.BookExists(link)
		if err != nil {
			return Result{}, err
		}
		if exists {
			return Result{}, ErrDuplicate
		}
	}

	sanTitle := Sanitize(title, false)
	today := time.Now().Format("2006-01-02")

	noteAbs := vault.ResolvePath(o.VaultDir, o.NewNoteDir)
	mdPath := filepath.Join(noteAbs, sanTitle+".md")
	if _, err := os.Stat(mdPath); err == nil {
		return Result{}, fmt.Errorf("%w: %s.md", ErrNoteExists, sanTitle)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Result{}, err
	}

	coverName := ""
	if coverURL != "" {
		coverName = "cover_" + Sanitize(title, true) + coverExt(coverURL)
		attachAbs := vault.ResolvePath(o.VaultDir, o.AttachmentsDir)
		if err := os.MkdirAll(attachAbs, 0o755); err != nil {
			return Result{}, err
		}
		if err := download(coverURL, filepath.Join(attachAbs, coverName)); err != nil {
			return Result{}, fmt.Errorf("cover download: %w", err)
		}
	}

	if err := os.MkdirAll(noteAbs, 0o755); err != nil {
		return Result{}, err
	}
	content := BuildBookNote(sanTitle, author, link, workID, coverName, status, "", description, today)
	if err := vault.AtomicWrite(mdPath, []byte(content), 0o644); err != nil {
		return Result{}, err
	}

	return Result{Path: mdPath, Title: sanTitle, Cover: coverName}, nil
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

	// Refuse to overwrite an existing note. Checked before the cover download so
	// a duplicate filename neither clobbers the note nor its cover. Stat errors
	// other than "not exist" are surfaced rather than assumed safe.
	noteAbs := vault.ResolvePath(o.VaultDir, o.NewNoteDir)
	mdPath := filepath.Join(noteAbs, title+".md")
	if _, err := os.Stat(mdPath); err == nil {
		return Result{}, fmt.Errorf("%w: %s.md", ErrNoteExists, title)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Result{}, err
	}

	// Download cover.
	coverName := "cover_" + Sanitize(nd.Title, true) + coverExt(nd.CoverURL)
	attachAbs := vault.ResolvePath(o.VaultDir, o.AttachmentsDir)
	if err := os.MkdirAll(attachAbs, 0o755); err != nil {
		return Result{}, err
	}
	if err := download(nd.CoverURL, filepath.Join(attachAbs, coverName)); err != nil {
		return Result{}, fmt.Errorf("cover download: %w", err)
	}

	// Write note atomically (temp + rename) so a crash mid-write can't leave a
	// half-written note.
	if err := os.MkdirAll(noteAbs, 0o755); err != nil {
		return Result{}, err
	}
	content := BuildNote(nd, sourceURL, coverName, today)
	if err := vault.AtomicWrite(mdPath, []byte(content), 0o644); err != nil {
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
	// Guarded client: the cover URL comes from a scraped page, so a malicious
	// source could point it at an internal address — same SSRF guard as fetch.
	client := scraper.NewGuardedHTTPClient(30 * time.Second)
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
	_, err = io.Copy(f, io.LimitReader(resp.Body, maxCoverBytes))
	return err
}
