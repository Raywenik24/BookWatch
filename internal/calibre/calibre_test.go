package calibre

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

// buildFixture writes a small metadata.db under libraryRoot mirroring the subset
// of the Calibre schema that Read touches, then populates two books:
//
//	1 — a Light Novel: series, two authors, eng+pol, isbn+lubimyczytac ids,
//	    HTML comments, has_cover, junk pubdate (year 0101).
//	2 — a bare book: no series, one author, no tags/ids/comments, no cover.
func buildFixture(t *testing.T, libraryRoot string) {
	t.Helper()
	if err := os.MkdirAll(libraryRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(libraryRoot, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT, sort TEXT,
		author_sort TEXT, uuid TEXT, path TEXT, has_cover INTEGER,
		series_index REAL, pubdate TEXT, timestamp TEXT);
	CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT, sort TEXT);
	CREATE TABLE books_authors_link (id INTEGER PRIMARY KEY, book INTEGER, author INTEGER);
	CREATE TABLE series (id INTEGER PRIMARY KEY, name TEXT, sort TEXT);
	CREATE TABLE books_series_link (id INTEGER PRIMARY KEY, book INTEGER, series INTEGER);
	CREATE TABLE languages (id INTEGER PRIMARY KEY, lang_code TEXT);
	CREATE TABLE books_languages_link (id INTEGER PRIMARY KEY, book INTEGER, lang_code INTEGER, item_order INTEGER);
	CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT);
	CREATE TABLE books_tags_link (id INTEGER PRIMARY KEY, book INTEGER, tag INTEGER);
	CREATE TABLE identifiers (id INTEGER PRIMARY KEY, book INTEGER, type TEXT, val TEXT);
	CREATE TABLE comments (id INTEGER PRIMARY KEY, book INTEGER, text TEXT);`
	exec(t, db, schema)

	// Book 1 — full record. path uses forward slashes as Calibre stores them.
	exec(t, db, `INSERT INTO books VALUES
		(1,'Volume One','Volume One','Tanaka, Aki & Sato, Ken','uuid-1',
		 'Tanaka Aki/Volume One (1)',1,1.0,'0101-01-01 00:00:00+00:00','2021-05-01 12:00:00+00:00'),
		(2,'Loner','Loner','Nobody','uuid-2','Nobody/Loner (2)',0,1.0,
		 '2020-01-01 00:00:00+00:00','2020-01-02 00:00:00+00:00')`)

	exec(t, db, `INSERT INTO authors VALUES (1,'Aki Tanaka','Tanaka, Aki'),
		(2,'Ken Sato','Sato, Ken'),(3,'Nobody','Nobody')`)
	// Two authors for book 1, order via link id.
	exec(t, db, `INSERT INTO books_authors_link VALUES (1,1,1),(2,1,2),(3,2,3)`)

	exec(t, db, `INSERT INTO series VALUES (1,'The Chronicle','Chronicle, The')`)
	exec(t, db, `INSERT INTO books_series_link VALUES (1,1,1)`)

	exec(t, db, `INSERT INTO languages VALUES (1,'eng'),(2,'pol')`)
	exec(t, db, `INSERT INTO books_languages_link VALUES (1,1,1,0),(2,1,2,1)`)

	exec(t, db, `INSERT INTO tags VALUES (1,'Light Novel'),(2,'Done')`)
	exec(t, db, `INSERT INTO books_tags_link VALUES (1,1,1),(2,1,2)`)

	exec(t, db, `INSERT INTO identifiers VALUES
		(1,1,'isbn','9781234567890'),(2,1,'lubimyczytac','54321')`)

	exec(t, db, `INSERT INTO comments VALUES
		(1,1,'<div><p>First &amp; foremost.</p><p>Line two.<br>Same para break.</p></div>')`)
}

func exec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func TestRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Calibre Library")
	buildFixture(t, root)

	books, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(books) != 2 {
		t.Fatalf("want 2 books, got %d", len(books))
	}

	b1, b2 := books[0], books[1]
	if b1.ID != 1 || b2.ID != 2 {
		t.Fatalf("books not sorted by id: %d, %d", b1.ID, b2.ID)
	}

	// Book 1 — full record.
	if b1.UUID != "uuid-1" || b1.Title != "Volume One" || b1.AuthorSort != "Tanaka, Aki & Sato, Ken" {
		t.Errorf("book1 core fields: %+v", b1)
	}
	if want := []string{"Aki Tanaka", "Ken Sato"}; !reflect.DeepEqual(b1.Authors, want) {
		t.Errorf("book1 authors = %v, want %v", b1.Authors, want)
	}
	if b1.Series != "The Chronicle" || b1.SeriesIndex != 1.0 {
		t.Errorf("book1 series = %q idx %v", b1.Series, b1.SeriesIndex)
	}
	if want := []string{"eng", "pol"}; !reflect.DeepEqual(b1.Languages, want) {
		t.Errorf("book1 languages = %v, want %v", b1.Languages, want)
	}
	if want := []string{"Done", "Light Novel"}; !reflect.DeepEqual(b1.Tags, want) {
		t.Errorf("book1 tags = %v, want %v", b1.Tags, want)
	}
	if b1.Identifiers["isbn"] != "9781234567890" || b1.Identifiers["lubimyczytac"] != "54321" {
		t.Errorf("book1 identifiers = %v", b1.Identifiers)
	}
	wantComment := "First & foremost.\nLine two.\nSame para break."
	if b1.Comments != wantComment {
		t.Errorf("book1 comments = %q, want %q", b1.Comments, wantComment)
	}
	if !b1.HasCover {
		t.Error("book1 should have a cover")
	}
	wantCover := filepath.Join(root, "Tanaka Aki", "Volume One (1)", "cover.jpg")
	if b1.CoverPath != wantCover {
		t.Errorf("book1 cover = %q, want %q", b1.CoverPath, wantCover)
	}
	// Junk pubdate is surfaced raw, not sanitized.
	if b1.PubDate != "0101-01-01 00:00:00+00:00" {
		t.Errorf("book1 pubdate = %q, want raw junk", b1.PubDate)
	}
	if b1.Timestamp != "2021-05-01 12:00:00+00:00" {
		t.Errorf("book1 timestamp = %q", b1.Timestamp)
	}

	// Book 2 — bare record: empty slices/maps, no cover.
	if b2.Series != "" {
		t.Errorf("book2 series should be empty, got %q", b2.Series)
	}
	if len(b2.Tags) != 0 || len(b2.Identifiers) != 0 || b2.Comments != "" {
		t.Errorf("book2 should have no tags/ids/comments: %+v", b2)
	}
	if want := []string{"Nobody"}; !reflect.DeepEqual(b2.Authors, want) {
		t.Errorf("book2 authors = %v, want %v", b2.Authors, want)
	}
	if b2.HasCover || b2.CoverPath != "" {
		t.Errorf("book2 should have no cover, got %q", b2.CoverPath)
	}
}

// TestReadIsReadOnly proves Read never mutates the database file: opening it and
// then writing through a separate read-only handle must fail.
func TestReadIsReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lib")
	buildFixture(t, root)
	if _, err := Read(root); err != nil {
		t.Fatalf("Read: %v", err)
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(root, "metadata.db")) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO tags VALUES (99,'x')`); err == nil {
		t.Error("expected write to read-only db to fail")
	}
}

func TestReadMissingDB(t *testing.T) {
	if _, err := Read(t.TempDir()); err == nil {
		t.Error("expected error reading a directory with no metadata.db")
	}
}

func TestPlainText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"<p>a</p><p>b</p>", "a\nb"},
		{"one<br>two", "one\ntwo"},
		{"<p>a</p><p>b</p>", "a\nb"},
		{"<p>x</p>\n\n\n<p>y</p>", "x\n\ny"}, // runs of blank lines collapse to one
		{"a &amp; b &lt;c&gt;", "a & b <c>"},
		{"<b>bold</b> text", "bold text"},
	}
	for _, c := range cases {
		if got := plainText(c.in); got != c.want {
			t.Errorf("plainText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
