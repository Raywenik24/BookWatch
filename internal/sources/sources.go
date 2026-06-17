// Package sources resolves a book URL to a scraper.Rules set, using the
// DB-stored sources (matched by domain) and falling back to the jnovels
// defaults. This is the extension point for adding new sites as data.
package sources

import (
	"net/url"
	"regexp"
	"strings"

	"pagewatch/internal/scraper"
	"pagewatch/internal/store"
)

type Resolver struct {
	byDomain map[string]scraper.Rules
}

// NewResolver builds a resolver from the DB. st may be nil (defaults only).
func NewResolver(st *store.Store) *Resolver {
	r := &Resolver{byDomain: map[string]scraper.Rules{}}
	if st == nil {
		return r
	}
	srcs, err := st.ListSources()
	if err != nil {
		return r
	}
	for _, s := range srcs {
		if s.Enabled && s.Strategy == "rules" {
			r.byDomain[strings.ToLower(s.Domain)] = BuildRules(s.Rules)
		}
	}
	return r
}

// For returns the rules for a URL's domain, or the defaults.
func (r *Resolver) For(rawURL string) scraper.Rules {
	host := domainOf(rawURL)
	for domain, rules := range r.byDomain {
		if domain != "" && strings.Contains(host, domain) {
			return rules
		}
	}
	return scraper.DefaultRules()
}

// BuildRules layers DB rule rows over the defaults.
func BuildRules(rules []store.Rule) scraper.Rules {
	out := scraper.DefaultRules()
	for _, r := range rules {
		switch r.Field {
		case "volume_list":
			if r.Selector != "" {
				out.VolumeListSel = r.Selector
			}
		case "volume_item":
			if r.Selector != "" {
				out.VolumeItemSel = r.Selector
			}
			if r.Regex != "" {
				if re, err := regexp.Compile(r.Regex); err == nil {
					out.VolumeItemRE = re
				}
			}
		case "title":
			if r.Selector != "" {
				out.TitleSel = r.Selector
			}
		case "cover":
			if r.Selector != "" {
				out.CoverSel = r.Selector
			}
			if r.Attr != "" {
				out.CoverAttr = r.Attr
			}
		case "description":
			if sels := splitSelectors(r.Selector); len(sels) > 0 {
				out.DescSels = sels
			}
		}
	}
	return out
}

// splitSelectors splits an ordered "a || b || c" fallback list.
func splitSelectors(s string) []string {
	var out []string
	for _, part := range strings.Split(s, "||") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func domainOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
