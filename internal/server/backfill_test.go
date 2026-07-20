package server

import "testing"

// deriveVolumeURL must rebuild jnovels' exact per-volume post URL from a series
// page URL (#108) — the slug pattern that lets backfill resolve "Ex" spinoffs the
// title search can't tell apart from the main series.
func TestDeriveVolumeURL(t *testing.T) {
	cases := []struct {
		name   string
		series string
		vol    int
		want   string
	}{
		{
			name:   "ex spinoff series page",
			series: "https://jnovels.com/so-im-a-spider-so-what-ex-light-novel-epub/",
			vol:    2,
			want:   "https://jnovels.com/so-im-a-spider-so-what-ex-volume-2-epub/",
		},
		{
			name:   "plain -epub series page",
			series: "https://jnovels.com/kings-proposal-light-novel-epub/",
			vol:    3,
			want:   "https://jnovels.com/kings-proposal-volume-3-epub/",
		},
		{
			name:   "no trailing slash",
			series: "https://jnovels.com/goblin-slayer-light-novel-epub",
			vol:    15,
			want:   "https://jnovels.com/goblin-slayer-volume-15-epub/",
		},
		{
			name:   "empty link falls through to search",
			series: "",
			vol:    1,
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveVolumeURL(tc.series, tc.vol); got != tc.want {
				t.Errorf("deriveVolumeURL(%q, %d) = %q, want %q", tc.series, tc.vol, got, tc.want)
			}
		})
	}
}
