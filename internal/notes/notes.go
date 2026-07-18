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
	// allVolSufRE strips jnovels' " all volumes" aggregate-page tag from a scraped
	// title ("Kumo Desu ga Nani ka all volumes" → "Kumo Desu ga Nani ka"), the same
	// way the "Download" lead-in and the format suffix are dropped. Applied after
	// the format suffix so "… all volumes Epub" reduces fully.
	allVolSufRE = regexp.MustCompile(`(?i)\s+all volumes$`)
	badCharRE   = regexp.MustCompile(`[<>"/\\|?*]`)
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
	title = allVolSufRE.ReplaceAllString(title, "")
	title = strings.ReplaceAll(title, ":", " -")
	title = badCharRE.ReplaceAllString(title, "")
	title = strings.TrimSpace(title)
	if removeSpaces {
		title = strings.ReplaceAll(title, " ", "")
	}
	return title
}

// BuildNote renders the markdown for a new LN note (frontmatter + body).
// status may be empty to fall back to the "Backlog" default (#52).
func BuildNote(nd scraper.NovelData, sourceURL, coverName, status, today string) string {
	title := Sanitize(nd.Title, false)
	if status == "" {
		status = "Backlog"
	}
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
  - %s
Template_used: LightNovelTemplate
Series Status:
created: %s
modified: %s
---
### %s

![[%s]]

[[Light Novel]]

%s
`, title, sourceURL, nd.Volumes, coverName, status, today, today, title, coverName, nd.Description)
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

// CoverName builds a cover attachment filename for a title + extension:
// "cover_<TitleNoSpaces><ext>". ext must include the leading dot. Shared by note
// creation, cover replacement, and title rename so the naming stays consistent.
func CoverName(title, ext string) string {
	return "cover_" + Sanitize(title, true) + ext
}

// CoverExt returns the file extension to use for a cover downloaded from
// coverURL (defaulting to .jpg). Exported for the cover-replace endpoint.
func CoverExt(coverURL string) string { return coverExt(coverURL) }

// SaveCoverBytes writes an uploaded cover image to dest, capped at
// maxCoverBytes (matching the download path). The parent dir is created.
func SaveCoverBytes(dest string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(r, maxCoverBytes))
	return err
}

// DownloadCover downloads coverURL to dest via the guarded (SSRF-safe) client,
// same as note creation uses. Unlike the internal download(), it rejects a
// response whose Content-Type isn't an image — the paste-URL path is
// user-facing, so a link to an HTML page (or a 404 page) should fail loudly
// rather than save junk as the cover.
func DownloadCover(coverURL, dest string) error {
	client := scraper.NewGuardedHTTPClient(30 * time.Second)
	resp, err := client.Get(coverURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		return fmt.Errorf("that URL is not an image")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(resp.Body, maxCoverBytes))
	return err
}

// RenameNote renames a note's .md to match newTitle, and — when the note has a
// cover — renames the cover attachment to follow (cover_<newTitle><ext>),
// rewriting the in-body H3 heading and the Cover field/embed to match. It
// refuses to clobber an existing note or cover (ErrNoteExists). Returns the new
// note path and cover filename (unchanged cover name if the note had none).
func RenameNote(o Options, oldPath, oldCover, newTitle string) (newPath, newCover string, err error) {
	newBase := Sanitize(newTitle, false)
	if newBase == "" {
		return "", "", errors.New("empty title")
	}
	newPath = filepath.Join(filepath.Dir(oldPath), newBase+".md")
	sameNote := strings.EqualFold(newPath, oldPath)
	if !sameNote {
		if _, e := os.Stat(newPath); e == nil {
			return "", "", fmt.Errorf("%w: %s.md", ErrNoteExists, newBase)
		} else if !errors.Is(e, fs.ErrNotExist) {
			return "", "", e
		}
	}

	// Rename the cover first (if present on disk) so a failure leaves the note
	// untouched; a recorded-but-missing cover is left as-is rather than failing.
	newCover = oldCover
	if oldCover != "" {
		attachAbs := vault.ResolvePath(o.VaultDir, o.AttachmentsDir)
		newCover = CoverName(newTitle, filepath.Ext(oldCover))
		oldCoverPath := filepath.Join(attachAbs, oldCover)
		newCoverPath := filepath.Join(attachAbs, newCover)
		if newCover != oldCover {
			if _, statErr := os.Stat(oldCoverPath); statErr == nil {
				if _, e := os.Stat(newCoverPath); e == nil && !strings.EqualFold(newCoverPath, oldCoverPath) {
					return "", "", fmt.Errorf("%w: %s", ErrNoteExists, newCover)
				}
				if e := vault.RenameWithRetry(oldCoverPath, newCoverPath); e != nil {
					return "", "", fmt.Errorf("cover rename: %w", e)
				}
			} else {
				newCover = oldCover
			}
		}
	}

	if !sameNote {
		if e := vault.RenameWithRetry(oldPath, newPath); e != nil {
			return "", "", e
		}
	}
	// Keep the in-body heading and (book notes) the `Title:` frontmatter field in
	// step with the new filename.
	if e := vault.SetTitleHeading(newPath, newBase); e != nil {
		return newPath, newCover, e
	}
	if e := vault.UpdateTitleField(newPath, newBase); e != nil {
		return newPath, newCover, e
	}
	if newCover != oldCover {
		if e := vault.UpdateCover(newPath, newCover); e != nil {
			return newPath, newCover, e
		}
	}
	return newPath, newCover, nil
}

// CreateBook writes a new #Book note from catalog data (OpenLibrary) — no
// scraping involved, unlike Create. coverURL/description/releasedEN may be
// empty. dup may be nil to skip the duplicate check.
func CreateBook(o Options, dup DupChecker, title, author, link, workID, coverURL, status, releasedEN, description string) (Result, error) {
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
		coverName = CoverName(title, coverExt(coverURL))
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
	content := BuildBookNote(sanTitle, author, link, workID, coverName, status, releasedEN, description, today)
	if err := vault.AtomicWrite(mdPath, []byte(content), 0o644); err != nil {
		return Result{}, err
	}

	return Result{Path: mdPath, Title: sanTitle, Cover: coverName}, nil
}

// Create scrapes the URL and writes a new note + cover into the vault.
// dup may be nil to skip the duplicate check. rl are the scrape rules.
// status may be empty to use the "Backlog" default (#52).
func Create(o Options, sc *scraper.Client, dup DupChecker, rl scraper.Rules, sourceURL, status string) (Result, error) {
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
	coverName := CoverName(nd.Title, coverExt(nd.CoverURL))
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
	content := BuildNote(nd, sourceURL, coverName, status, today)
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
