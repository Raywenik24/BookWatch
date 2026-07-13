package importer

import (
	"strings"
	"testing"

	"bookwatch/internal/calibre"
)

func idBook(title, czyj string) calibre.Book {
	b := calibre.Book{Title: title}
	if czyj != "" {
		b.Identifiers = map[string]string{"czyj": czyj}
	}
	return b
}

func titles(bs []calibre.Book) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Title
	}
	return out
}

func TestImportFilter(t *testing.T) {
	books := []calibre.Book{
		idBook("Andrzej's", "andrzej"),
		idBook("Arek's", "arek"),
		idBook("Andrzej caps", "Andrzej"),                 // case-insensitive value
		idBook("No owner", ""),                            // missing identifier
		idBook("Co-owned", "Mama; czyj:Andrzej"),          // semicolon + embedded prefix → {mama, andrzej}
		idBook("Two owners", "andrzej; czyj:arek"),        // → {andrzej, arek}
		idBook("Spaced", "andrzej arek"),                  // ONE value (space isn't a separator)
	}

	// Inactive (blank field) → everything passes, same slice.
	if got := (ImportFilter{}).Apply(books); len(got) != len(books) {
		t.Errorf("inactive filter dropped books: %v", titles(got))
	}

	// czyj=andrzej: exact 'andrzej' + case variant + the two multi-owner books
	// (comma/semicolon split, embedded prefix stripped). NOT the space-joined one.
	f := ImportFilter{Field: "czyj", Values: []string{"andrzej"}}
	got := titles(f.Apply(books))
	want := []string{"Andrzej's", "Andrzej caps", "Co-owned", "Two owners"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("andrzej filter = %v, want %v", got, want)
	}

	// The space-joined "andrzej arek" is a single value → excluded for 'andrzej'.
	for _, tl := range got {
		if tl == "Spaced" {
			t.Error("space-separated value should NOT match a single owner")
		}
	}

	// Include-missing also admits the book with no czyj identifier.
	f.IncludeMissing = true
	if got := titles(f.Apply(books)); !contains(strings.Join(got, "|"), "No owner") {
		t.Errorf("include-missing = %v, want +No owner", got)
	}

	// arek accepts both the plain 'arek' and the "andrzej; czyj:arek" book.
	got = titles(ImportFilter{Field: "czyj", Values: []string{"arek"}}.Apply(books))
	want = []string{"Arek's", "Two owners"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("arek filter = %v, want %v", got, want)
	}

	// Field the books don't carry, without include-missing → nothing.
	if got := (ImportFilter{Field: "owner", Values: []string{"x"}}).Apply(books); len(got) != 0 {
		t.Errorf("unknown field = %v, want none", titles(got))
	}

	// Field set with no accepted Values is a presence filter: every book
	// carrying the identifier passes, regardless of its value.
	got = titles(ImportFilter{Field: "czyj"}.Apply(books))
	want = []string{"Andrzej's", "Arek's", "Andrzej caps", "Co-owned", "Two owners", "Spaced"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("presence filter (no values) = %v, want %v", got, want)
	}

	// Same presence filter with include-missing → all books, including "No owner".
	got = titles(ImportFilter{Field: "czyj", IncludeMissing: true}.Apply(books))
	if len(got) != len(books) {
		t.Errorf("presence filter + include-missing = %v, want all %d books", got, len(books))
	}
}

func TestSplitFilterValues(t *testing.T) {
	got := SplitFilterValues(" andrzej,  arek ;\n bob \n")
	if len(got) != 3 || got[0] != "andrzej" || got[1] != "arek" || got[2] != "bob" {
		t.Errorf("SplitFilterValues = %#v", got)
	}
	if got := SplitFilterValues("   "); len(got) != 0 {
		t.Errorf("blank should yield no values, got %#v", got)
	}
}
