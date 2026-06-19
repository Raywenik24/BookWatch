package scraper

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const novelPage = `<!doctype html><html><body>
<h1 class="post-title entry-title">Test Novel Epub</h1>
<div class="featured-media"><img src="/covers/test.jpg"></div>
<div class="synopsis-description"><p>Line one.</p><p>Line two.</p></div>
<ol><li>Intro (no volume)</li></ol>
<ol>
  <li>Download VOLUME 1 Epub</li>
  <li>Download VOLUME 2 Epub</li>
  <li>Download VOLUME 3 Epub</li>
</ol>
</body></html>`

func TestParseNovelHTML_defaults(t *testing.T) {
	nd, err := ParseNovelHTML(novelPage, DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if nd.Title != "Test Novel Epub" {
		t.Errorf("title %q", nd.Title)
	}
	if nd.Volumes != 3 {
		t.Errorf("volumes %d (last <ol> should win)", nd.Volumes)
	}
	if nd.CoverURL != "/covers/test.jpg" {
		t.Errorf("cover %q", nd.CoverURL)
	}
	if nd.Description != "Line one. Line two." {
		t.Errorf("desc %q", nd.Description)
	}
}

func TestParseNovelHTML_missingTitle(t *testing.T) {
	if _, err := ParseNovelHTML(`<html><body><ol><li>VOLUME 1</li></ol></body></html>`, DefaultRules()); err == nil {
		t.Error("expected error on missing title")
	}
}

func TestParseNovelHTML_missingCover(t *testing.T) {
	if _, err := ParseNovelHTML(`<html><body><h1 class="post-title entry-title">T</h1></body></html>`, DefaultRules()); err == nil {
		t.Error("expected error on missing cover")
	}
}

func TestLatestVolume_okAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte(novelPage))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New("test-agent", 5*time.Second)

	max, count, err := c.LatestVolume(srv.URL+"/ok", DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if max != 3 {
		t.Errorf("max %d", max)
	}
	if count != 3 {
		t.Errorf("count %d", count)
	}
	if _, _, err := c.LatestVolume(srv.URL+"/404", DefaultRules()); err == nil {
		t.Error("expected error on non-200")
	}
}

func TestLatestVolume_timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(novelPage))
	}))
	defer srv.Close()
	c := New("t", 30*time.Millisecond)
	if _, _, err := c.LatestVolume(srv.URL, DefaultRules()); err == nil {
		t.Error("expected a timeout error")
	}
}
