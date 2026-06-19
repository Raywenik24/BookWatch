package sources

import (
	"testing"

	"bookwatch/internal/scraper"
	"bookwatch/internal/store"
)

func TestBuildRules_overridesAndDefaults(t *testing.T) {
	out := BuildRules([]store.Rule{
		{Field: "title", Selector: "h2.custom"},
		{Field: "volume_item", Selector: "li.v", Regex: `(?i)vol (\d+)`},
		{Field: "cover", Selector: "img.c", Attr: "data-src"},
		{Field: "description", Selector: "div.a || #b || .c"},
		{Field: "volume_list", Selector: ""}, // empty must NOT override the default
	})
	if out.TitleSel != "h2.custom" {
		t.Errorf("title: %q", out.TitleSel)
	}
	if out.VolumeItemSel != "li.v" {
		t.Errorf("item: %q", out.VolumeItemSel)
	}
	if out.VolumeItemRE == nil || !out.VolumeItemRE.MatchString("Vol 7") {
		t.Error("regex not compiled/applied")
	}
	if out.CoverSel != "img.c" || out.CoverAttr != "data-src" {
		t.Errorf("cover: %q %q", out.CoverSel, out.CoverAttr)
	}
	if len(out.DescSels) != 3 || out.DescSels[1] != "#b" {
		t.Errorf("desc sels: %+v", out.DescSels)
	}
	if out.VolumeListSel != "ol" {
		t.Errorf("empty selector overrode the default: %q", out.VolumeListSel)
	}
}

func TestBuildRules_badRegexLeavesDefault(t *testing.T) {
	out := BuildRules([]store.Rule{{Field: "volume_item", Regex: `(unclosed`}})
	if out.VolumeItemRE == nil {
		t.Error("a bad regex should leave the default VolumeRE, not nil")
	}
}

func TestResolver_ForDomainMatchAndFallback(t *testing.T) {
	r := &Resolver{byDomain: map[string]scraper.Rules{
		"foo.com": {TitleSel: "h9"},
	}}
	if got := r.For("https://www.foo.com/book"); got.TitleSel != "h9" {
		t.Errorf("domain match failed: %q", got.TitleSel)
	}
	if got := r.For("https://other.com/x"); got.TitleSel != scraper.DefaultRules().TitleSel {
		t.Errorf("unknown domain should fall back to defaults: %q", got.TitleSel)
	}
}

func TestSplitSelectors(t *testing.T) {
	got := splitSelectors(" a || b ||  || c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v want %v", got, want)
		}
	}
}

func TestDomainOf(t *testing.T) {
	if d := domainOf("https://WWW.Example.COM/path"); d != "www.example.com" {
		t.Errorf("got %q", d)
	}
}
