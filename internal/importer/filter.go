package importer

import (
	"strings"

	"bookwatch/internal/calibre"
)

// ImportFilter restricts which Calibre books an import covers, keyed on a single
// identifier field (a Calibre `type:value` identifier, e.g. `czyj:andrzej`). It
// runs before grouping/matching, in both the dry-run preview and the real run,
// so an owner-scoped library imports only the wanted books.
//
// A blank Field disables the filter — every book passes (the default). Otherwise
// a book passes when *any* owner in its Field value is one of Values, OR the book
// has no Field identifier at all and IncludeMissing is set.
//
// Calibre only stores one value per identifier type, so people pack several
// owners into that one value. This treats **comma and semicolon** as owner
// separators (`andrzej; czyj:arek` → owners {andrzej, arek}; an embedded
// `czyj:` prefix, which is how Calibre mangles a second same-type id, is
// stripped) but **not** whitespace — `andrzej arek` (space) is one owner value
// and won't match `andrzej`. All comparisons are case-insensitive, trimmed.
type ImportFilter struct {
	Field          string
	Values         []string
	IncludeMissing bool
}

// Active reports whether the filter does anything (a non-blank field).
func (f ImportFilter) Active() bool { return strings.TrimSpace(f.Field) != "" }

// Apply returns the subset of books the filter admits. An inactive filter
// returns books unchanged. The input slice is never mutated.
func (f ImportFilter) Apply(books []calibre.Book) []calibre.Book {
	if !f.Active() {
		return books
	}
	field := strings.TrimSpace(f.Field)
	accept := make(map[string]bool, len(f.Values))
	for _, v := range f.Values {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			accept[v] = true
		}
	}
	out := make([]calibre.Book, 0, len(books))
	for _, b := range books {
		val, has := identValue(b, field)
		if !has {
			if f.IncludeMissing {
				out = append(out, b)
			}
			continue
		}
		for _, owner := range splitOwners(val, field) {
			if accept[strings.ToLower(owner)] {
				out = append(out, b)
				break
			}
		}
	}
	return out
}

// splitOwners parses a Calibre identifier value into its owner tokens: split on
// comma/semicolon only (never whitespace), each token stripped of an embedded
// `<field>:` prefix and surrounding space. So `andrzej; czyj:arek` → [andrzej,
// arek] while `andrzej arek` → [andrzej arek] (one token).
func splitOwners(val, field string) []string {
	prefix := strings.ToLower(strings.TrimSpace(field)) + ":"
	parts := strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(strings.ToLower(p), prefix) {
			p = strings.TrimSpace(p[len(prefix):])
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// identValue looks up a book's identifier by type, case-insensitively (Calibre
// stores types lowercased, but be forgiving about the configured field's case).
func identValue(b calibre.Book, field string) (string, bool) {
	for k, v := range b.Identifiers {
		if strings.EqualFold(k, field) {
			return v, true
		}
	}
	return "", false
}

// SplitFilterValues parses the settings string (comma / semicolon / newline
// separated) into the accepted-value list.
func SplitFilterValues(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
