package scraper

import "testing"

func TestNormalizeApostrophes(t *testing.T) {
	cases := map[string]string{
		"That Time I Got Reincarnated": "That Time I Got Reincarnated", // no-op
		"Marielle Clarac’s Musings":    "Marielle Clarac's Musings",
		"‘Quoted’":                     "'Quoted'",
		"Back`tick":                    "Back'tick",
	}
	for in, want := range cases {
		if got := NormalizeApostrophes(in); got != want {
			t.Errorf("NormalizeApostrophes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchQueryText(t *testing.T) {
	cases := map[string]string{
		"So I’m a Spider, So What?": "So I'm a Spider So What", // curly ' kept, comma + ? dropped
		"Kumo Desu ga Nani ka":      "Kumo Desu ga Nani ka",    // no-op
		"Re:Zero − Starting Life":   "Re Zero Starting Life",   // colon + dash → spaces
		"  extra   spaces  ":        "extra spaces",
	}
	for in, want := range cases {
		if got := searchQueryText(in); got != want {
			t.Errorf("searchQueryText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVolumeNumber(t *testing.T) {
	cases := map[string]int{
		"Kumo Desu ga Nani ka Volume 7":                   7,
		"Mushoku Tensei Redundant Reincarnation Volume 3": 3,
		"Overlord":              0, // aggregate, no volume
		"Some Novel Volume 012": 12,
	}
	for in, want := range cases {
		if got := volumeNumber(in); got != want {
			t.Errorf("volumeNumber(%q) = %d, want %d", in, got, want)
		}
	}
}

// VolumeMatch picks the post for the exact requested volume whose series portion
// clears the confidence gate; unrelated hits and wrong volumes are rejected.
func TestVolumeMatch(t *testing.T) {
	results := []SearchResult{
		{Title: "Mushoku Tensei Redundant Reincarnation", URL: "https://jnovels.com/mushoku-tensei-redundant-reincarnation-light-novel-epub/"}, // aggregate, vol 0
		{Title: "Mushoku Tensei Redundant Reincarnation Volume 3", URL: "https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-3-epub/"},
		{Title: "Mushoku Tensei Redundant Reincarnation Volume 2", URL: "https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-2-epub/"},
		{Title: "Semantic Error Volume 3", URL: "https://jnovels.com/semantic-error-volume-3-epub/"}, // unrelated, same vol number
	}

	got, ok := VolumeMatch(results, "Mushoku Tensei: Redundant Reincarnation", 3)
	if !ok {
		t.Fatal("expected a confident match for volume 3")
	}
	if got.URL != "https://jnovels.com/mushoku-tensei-redundant-reincarnation-volume-3-epub/" {
		t.Errorf("matched %q, want the volume-3 post", got.URL)
	}

	// A volume nobody has → no match.
	if _, ok := VolumeMatch(results, "Mushoku Tensei: Redundant Reincarnation", 9); ok {
		t.Error("expected no match for a missing volume")
	}

	// An unrelated series title → the confidence gate rejects the same-numbered hit.
	if _, ok := VolumeMatch(results, "Completely Different Series", 3); ok {
		t.Error("expected the confidence gate to reject an unrelated series")
	}
}

// The gate tolerates a differing subtitle as long as most series tokens survive.
func TestVolumeMatch_subtitleTolerance(t *testing.T) {
	results := []SearchResult{
		{Title: "Kumo Desu ga Nani ka Volume 7", URL: "https://jnovels.com/kumo-volume-7-epub/"},
	}
	// Missing the trailing "ka" token — still ≥2/3 of the series tokens present.
	if _, ok := VolumeMatch(results, "Kumo Desu ga Nani", 7); !ok {
		t.Error("expected a near-title to still clear the 2/3 gate")
	}
}

// jnovels often lists a volume under a different localized title than the series
// page ("Kumo Desu ga Nani ka" → "So I'm a Spider, So What?"). Its alias-aware
// search still returns the right post, so a *single* exact-volume candidate with
// no title overlap is trusted — but two ambiguous ones are not.
func TestVolumeMatch_aliasTitle(t *testing.T) {
	single := []SearchResult{
		{Title: "Kumo Desu ga Nani ka all volumes", URL: "https://jnovels.com/kumo-all-volumes-epub/"}, // aggregate, vol 0
		{Title: "So I’m a Spider, So What? Volume 16", URL: "https://jnovels.com/so-im-a-spider-so-what-volume-16-epub/"},
	}
	got, ok := VolumeMatch(single, "Kumo Desu ga Nani ka all volumes", 16)
	if !ok || got.URL != "https://jnovels.com/so-im-a-spider-so-what-volume-16-epub/" {
		t.Errorf("expected the sole exact-volume alias post to be trusted, got ok=%v %q", ok, got.URL)
	}

	// Two non-overlapping exact-volume candidates → ambiguous → no guess.
	ambiguous := append(single, SearchResult{Title: "Totally Unrelated Series Volume 16", URL: "https://jnovels.com/unrelated-volume-16-epub/"})
	if _, ok := VolumeMatch(ambiguous, "Kumo Desu ga Nani ka all volumes", 16); ok {
		t.Error("expected ambiguous non-overlapping candidates to be rejected")
	}
}
