package checker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bookwatch/internal/scraper"
	"bookwatch/internal/vault"
)

func init() { scraper.AllowPrivateHosts = true } // httptest binds to loopback

func novelHTML(vols int) string {
	items := ""
	for i := 1; i <= vols; i++ {
		items += fmt.Sprintf("<li>Download VOLUME %d Epub</li>", i)
	}
	return `<!doctype html><html><body>
<h1 class="post-title entry-title">Novel Epub</h1>
<div class="featured-media"><img src="/c.jpg"></div>
<div class="synopsis-description"><p>Desc.</p></div>
<ol>` + items + `</ol></body></html>`
}

func TestCheck_flagsNewAndCapturesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(novelHTML(3)))
	}))
	defer srv.Close()

	sc := scraper.New("t", 5*time.Second)
	resolve := func(string) scraper.Rules { return scraper.DefaultRules() }

	entries := []vault.Entry{
		{Title: "A", Link: srv.URL + "/a", Volumes: 2},   // 3 > 2 → new
		{Title: "B", Link: srv.URL + "/b", Volumes: 3},   // 3 == 3 → no new
		{Title: "C", Link: srv.URL + "/bad", Volumes: 1}, // error
	}

	var progressCalls int
	res := Check(entries, sc, resolve, func(i, total int, title string) { progressCalls++ })

	if len(res) != 3 {
		t.Fatalf("got %d results", len(res))
	}
	if !res[0].HasNew || res[0].Latest != 3 {
		t.Errorf("A should have new (latest=%d)", res[0].Latest)
	}
	if res[1].HasNew {
		t.Error("B should not have new")
	}
	if res[2].Err == nil {
		t.Error("C should carry an error")
	}
	if res[2].HasNew {
		t.Error("an errored entry must not be flagged HasNew")
	}
	if progressCalls != 3 {
		t.Errorf("progress called %d times, want 3", progressCalls)
	}
}

func TestCheck_flagsSuspiciousScrapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/broken":
			// 200, valid-ish page, but no volume list at all → count 0.
			w.Write([]byte(`<html><body><h1 class="post-title entry-title">X</h1></body></html>`))
		default:
			w.Write([]byte(novelHTML(3)))
		}
	}))
	defer srv.Close()

	sc := scraper.New("t", 5*time.Second)
	resolve := func(string) scraper.Rules { return scraper.DefaultRules() }

	entries := []vault.Entry{
		{Title: "Broken", Link: srv.URL + "/broken", Volumes: 5},     // count 0 → suspicious
		{Title: "Regress", Link: srv.URL + "/regress", Volumes: 10},  // reads 3 < 10 → suspicious
		{Title: "Healthy", Link: srv.URL + "/ok", Volumes: 3},        // reads 3 == 3 → fine
	}
	res := Check(entries, sc, resolve, nil)

	if !res[0].Suspicious || res[0].HasNew {
		t.Errorf("no-list page should be suspicious, not new: %+v", res[0])
	}
	if !res[1].Suspicious || res[1].HasNew {
		t.Errorf("volume regression should be suspicious: %+v", res[1])
	}
	if res[2].Suspicious {
		t.Errorf("a healthy unchanged book must not be flagged: %+v", res[2])
	}
}
