// Package calibre is a read-only reader for a Calibre metadata.db. It opens the
// database with mode=ro (never writes, never touches the network) and returns a
// clean Go model of the library: one Book per row with its authors, series,
// languages, tags, identifiers, stripped comments, and the on-disk cover path.
//
// It is the foundation for the Calibre import (milestone 1.2.0). Matching and
// vault writes live in later stages — this package only reads.
package calibre

import (
	"database/sql"
	"fmt"
	"html"
	"path/filepath"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"
)

// Book is one Calibre library entry. Fields mirror what the import needs; raw
// values (pubdate, timestamp) are surfaced verbatim so later stages can
// sanity-check junk dates like year 0101.
type Book struct {
	ID          int64
	UUID        string // stable idempotency key ("" only in very old libraries)
	Title       string
	AuthorSort  string
	Authors     []string          // in link order
	Series      string            // "" when the book is not in a series
	SeriesIndex float64           // only meaningful when Series != ""
	Languages   []string          // lang_code, e.g. "eng", "pol"
	Tags        []string          // e.g. "Light Novel", "Done"
	Identifiers map[string]string // type → value, e.g. "isbn" → "978…"
	Comments    string            // description, HTML stripped to plain text
	HasCover    bool
	CoverPath   string // absolute path to cover.jpg, "" when HasCover is false
	PubDate     string // raw, as stored (may be junk)
	Timestamp   string // raw, as stored
}

// Read opens <libraryRoot>/metadata.db read-only and returns every book, sorted
// by id. libraryRoot is the Calibre library folder (the one holding
// metadata.db); cover paths are resolved under it as <libraryRoot>/<path>/cover.jpg.
func Read(libraryRoot string) ([]Book, error) {
	dbPath := filepath.Join(libraryRoot, "metadata.db")
	// The file: URI form with mode=ro is what actually opens the SQLite file
	// read-only under modernc — a bare path with ?mode=ro is ignored and still
	// writable. ToSlash keeps Windows drive paths (C:\…) valid as a URI.
	dsn := "file:" + filepath.ToSlash(dbPath) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer db.Close()

	authors, err := multiStrings(db, `SELECT bal.book, a.name
		FROM books_authors_link bal JOIN authors a ON a.id = bal.author
		ORDER BY bal.book, bal.id`)
	if err != nil {
		return nil, fmt.Errorf("authors: %w", err)
	}
	langs, err := multiStrings(db, `SELECT bll.book, l.lang_code
		FROM books_languages_link bll JOIN languages l ON l.id = bll.lang_code
		ORDER BY bll.book, bll.item_order`)
	if err != nil {
		return nil, fmt.Errorf("languages: %w", err)
	}
	tags, err := multiStrings(db, `SELECT btl.book, t.name
		FROM books_tags_link btl JOIN tags t ON t.id = btl.tag
		ORDER BY btl.book, t.name`)
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	series, err := singleStrings(db, `SELECT bsl.book, s.name
		FROM books_series_link bsl JOIN series s ON s.id = bsl.series`)
	if err != nil {
		return nil, fmt.Errorf("series: %w", err)
	}
	comments, err := singleStrings(db, `SELECT book, text FROM comments`)
	if err != nil {
		return nil, fmt.Errorf("comments: %w", err)
	}
	idents, err := identifiers(db)
	if err != nil {
		return nil, fmt.Errorf("identifiers: %w", err)
	}

	rows, err := db.Query(`SELECT id, uuid, title, author_sort, series_index,
		path, has_cover, pubdate, timestamp FROM books ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("books: %w", err)
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var (
			b                      Book
			uuid, authorSort, path sql.NullString
			pubdate, timestamp     sql.NullString
			seriesIdx              sql.NullFloat64
			hasCover               sql.NullInt64
		)
		if err := rows.Scan(&b.ID, &uuid, &b.Title, &authorSort, &seriesIdx,
			&path, &hasCover, &pubdate, &timestamp); err != nil {
			return nil, fmt.Errorf("scan book: %w", err)
		}
		b.UUID = uuid.String
		b.AuthorSort = authorSort.String
		b.SeriesIndex = seriesIdx.Float64
		b.PubDate = pubdate.String
		b.Timestamp = timestamp.String
		b.Authors = authors[b.ID]
		b.Languages = langs[b.ID]
		b.Tags = tags[b.ID]
		b.Series = series[b.ID]
		b.Identifiers = idents[b.ID]
		b.Comments = plainText(comments[b.ID])
		b.HasCover = hasCover.Int64 != 0
		if b.HasCover && path.String != "" {
			b.CoverPath = filepath.Join(libraryRoot, filepath.FromSlash(path.String), "cover.jpg")
		}
		books = append(books, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("books: %w", err)
	}
	return books, nil
}

// multiStrings runs a two-column (book id, value) query and groups values per
// book, preserving row order.
func multiStrings(db *sql.DB, query string) (map[int64][]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = append(out[id], v)
	}
	return out, rows.Err()
}

// singleStrings runs a two-column (book id, value) query where each book has at
// most one row (series, comments).
func singleStrings(db *sql.DB, query string) (map[int64]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// identifiers groups the identifiers table into per-book type→value maps.
func identifiers(db *sql.DB) (map[int64]map[string]string, error) {
	rows, err := db.Query(`SELECT book, type, val FROM identifiers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]string{}
	for rows.Next() {
		var id int64
		var typ, val string
		if err := rows.Scan(&id, &typ, &val); err != nil {
			return nil, err
		}
		m := out[id]
		if m == nil {
			m = map[string]string{}
			out[id] = m
		}
		m[typ] = val
	}
	return out, rows.Err()
}

// ── HTML → plain text ─────────────────────────────────────────
// Calibre comments are stored as an HTML fragment. We strip them to plain text,
// turning <br> and block-close tags into line breaks so paragraph structure
// survives, then dropping any remaining markup and unescaping entities.

var (
	commentBrRE    = regexp.MustCompile(`(?i)<br\s*/?>`)
	commentBlockRE = regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|ul|ol|blockquote)>`)
	commentTagRE   = regexp.MustCompile(`<[^>]*>`)
)

func plainText(s string) string {
	if s == "" {
		return ""
	}
	s = commentBrRE.ReplaceAllString(s, "\n")
	s = commentBlockRE.ReplaceAllString(s, "\n")
	s = commentTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)

	// Collapse whitespace: trim each line, drop runs of blank lines.
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	// Drop leading/trailing blank lines.
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
