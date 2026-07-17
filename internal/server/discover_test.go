package server

import (
	"testing"

	"bookwatch/internal/scraper"
)

// filterFindNew must keep only epub light-novel picks — dropping pdf twins and
// manga (mangacbz) posts — and skip series already tracked.
func TestFilterFindNew(t *testing.T) {
	raw := []scraper.Release{
		{URL: "https://jnovels.com/goblin-slayer-volume-15-epub/"},           // keep
		{URL: "https://jnovels.com/tearmoon-empire-volume-10-pdf/"},          // drop: pdf
		{URL: "https://jnovels.com/mangacbz-ai-yori-aoshi/"},                 // drop: manga
		{URL: "https://jnovels.com/reincarnated-as-a-sword-volume-11-epub/"}, // drop: owned
		{URL: "https://jnovels.com/kings-proposal-light-novel-epub/"},        // keep
	}
	owned := map[string]bool{"reincarnated-as-a-sword": true}
	got := filterFindNew(raw, owned)
	if len(got) != 2 {
		t.Fatalf("got %d picks, want 2: %+v", len(got), got)
	}
	want := map[string]bool{
		"https://jnovels.com/goblin-slayer-volume-15-epub/":    true,
		"https://jnovels.com/kings-proposal-light-novel-epub/": true,
	}
	for _, r := range got {
		if !want[r.URL] {
			t.Errorf("unexpected pick kept: %q", r.URL)
		}
	}
}

// seriesKey must collapse a volume post and its series page to the same key so
// Find-new can skip a randomizer volume pick whose series is already tracked.
func TestSeriesKey(t *testing.T) {
	cases := map[string]string{
		"https://jnovels.com/goblin-slayer-volume-15-epub/":    "goblin-slayer",
		"https://jnovels.com/goblin-slayer-light-novel-epub/":  "goblin-slayer",
		"https://jnovels.com/tearmoon-empire-volume-10-pdf/":   "tearmoon-empire",
		"https://jnovels.com/kings-proposal-light-novel-epub/": "kings-proposal",
		"https://jnovels.com/some-standalone-epub/":            "some-standalone",
	}
	for in, want := range cases {
		if got := seriesKey(in); got != want {
			t.Errorf("seriesKey(%q) = %q, want %q", in, got, want)
		}
	}
	// A volume pick and the tracked series page collapse to one key.
	if seriesKey("https://jnovels.com/goblin-slayer-volume-15-epub/") !=
		seriesKey("https://jnovels.com/goblin-slayer-light-novel-epub/") {
		t.Error("volume pick and series page should share a series key")
	}
}

func TestDiscoverIsJnovelsURL(t *testing.T) {
	ok := []string{
		"https://jnovels.com/some-novel-epub/",
		"https://www.jnovels.com/x/",
	}
	bad := []string{
		"http://jnovels.com/x/",           // not https
		"https://evil.com/x/",             // wrong host
		"https://jnovels.com.evil.com/x/", // suffix trick
		"not a url",
	}
	for _, u := range ok {
		if !discoverIsJnovelsURL(u) {
			t.Errorf("expected %q to be a valid jnovels URL", u)
		}
	}
	for _, u := range bad {
		if discoverIsJnovelsURL(u) {
			t.Errorf("expected %q to be rejected", u)
		}
	}
}

func TestDiscoverIsCoverURL(t *testing.T) {
	ok := []string{
		"https://i0.wp.com/jnovels.com/wp-content/uploads/x.jpg",
		"https://jnovels.com/wp-content/uploads/x.jpg",
		"https://i1.wp.com/jnovels.com/y.png",
	}
	bad := []string{
		"http://i0.wp.com/x.jpg",  // not https
		"https://evil.com/x.jpg",  // wrong host
		"https://notwp.com/x.jpg", // wp.com suffix must be a real label boundary
	}
	for _, u := range ok {
		if !discoverIsCoverURL(u) {
			t.Errorf("expected cover URL %q to be allowed", u)
		}
	}
	for _, u := range bad {
		if discoverIsCoverURL(u) {
			t.Errorf("expected cover URL %q to be rejected", u)
		}
	}
}
