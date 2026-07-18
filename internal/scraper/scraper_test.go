package scraper

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// httptest binds to loopback, which the SSRF guard blocks by default — allow it
// for this package's fetch tests. The guard itself is exercised below with the
// flag flipped back off.
func init() { AllowPrivateHosts = true }

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1",
		"169.254.1.1", "0.0.0.0", "fe80::1", "fc00::1"}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestFetch_ssrfGuardBlocksLoopback(t *testing.T) {
	prev := AllowPrivateHosts
	AllowPrivateHosts = false
	defer func() { AllowPrivateHosts = prev }()

	c := New("t", 3*time.Second)
	_, _, err := c.LatestVolume("http://127.0.0.1:9/", DefaultRules())
	if err == nil {
		t.Fatal("expected the SSRF guard to block a loopback fetch")
	}
	if !strings.Contains(err.Error(), "ssrf guard") {
		t.Errorf("expected an ssrf-guard error, got: %v", err)
	}
}

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

// Older jnovels pages have no synopsis container — the blurb is loose <p>s
// after a `<p><b>SYNOPSIS</b></p>` label, ending at an "Associated Names"
// heading (verified live on der-werwolf-the-annals-of-veight-epub). The
// synopsisAfterLabel fallback must recover it, and stop before the heading.
const novelPageOldLayout = `<!doctype html><html><body>
<h1 class="post-title entry-title">Der Werwolf Epub</h1>
<div class="post-content">
  <div class="featured-media"><img src="/covers/w.jpg"></div>
  <p><b>SYNOPSIS</b></p>
  <p>The reborn werewolf known as Veight now leads the regiment.</p>
  <p>He seeks peace between humans and demons.</p>
  <h5 class="seriesother associated">Associated Names</h5>
  <div id="editassociated">Jinrou e no Tensei</div>
  <ol>
    <li>VOLUME 01 Epub</li>
    <li>VOLUME 02 Epub</li>
  </ol>
</div>
</body></html>`

func TestParseNovelHTML_synopsisLabelFallback(t *testing.T) {
	nd, err := ParseNovelHTML(novelPageOldLayout, DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	want := "The reborn werewolf known as Veight now leads the regiment. He seeks peace between humans and demons."
	if nd.Description != want {
		t.Errorf("desc %q, want %q", nd.Description, want)
	}
	if nd.Volumes != 2 {
		t.Errorf("volumes %d, want 2", nd.Volumes)
	}
}

// jnovels pages sometimes put the label inline as a bold/span prefix inside
// the same <p> as the blurb (verified live on
// maou-no-ore-ga-dorei-elf-wo-yome-ni-shitanda-ga-dou-medereba-), instead of
// a standalone label <p> followed by blurb <p>s. Neither DescSels nor
// synopsisAfterLabel match this shape.
const novelPageInlineLabel = `<!doctype html><html><body>
<h1 class="post-title entry-title">Maou no Ore ga Dorei Elf Epub</h1>
<div class="post-content">
  <div class="featured-media"><img src="/covers/m.jpg"></div>
  <p><span style="color:#ff0000;"><b>Description</b></span><br />
  Zagan is feared as an evil mage, but all he wants is a quiet life.</p>
  <p><b>Associated Names</b><br />Maou no Ore ga Dorei Elf</p>
  <ol>
    <li>VOLUME 01 Epub</li>
  </ol>
</div>
</body></html>`

func TestParseNovelHTML_inlineLabelParagraphFallback(t *testing.T) {
	nd, err := ParseNovelHTML(novelPageInlineLabel, DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	want := "Zagan is feared as an evil mage, but all he wants is a quiet life."
	if nd.Description != want {
		t.Errorf("desc %q, want %q", nd.Description, want)
	}
}

// jnovels' newer per-volume posts wrap the synopsis in a Kobo collapsible widget:
// the blurb sits as a bare text node inside a *nested* div.synopsis-description
// (no <p> to find), and an outer div.synopsis-description wraps the widget. The
// old ".First() + <p>-only" extraction returned "" here (verified live on
// who-killed-the-hero-tale-of-the-prophecy-volume-2); the directText fallback
// must recover the blurb.
const novelPageKoboWidget = `<!doctype html><html><body>
<h1 class="post-title entry-title">Who Killed the Hero Volume 2 Epub</h1>
<div class="featured-media"><img src="/covers/w.jpg"></div>
<div class="synopsis-description">
  <div id="synopsis" class="item-synopsis">
    <div class="collapsible-synopsis" data-kobo-gizmo="Collapsible.Synopsis">
      <div class="synopsis-description">Tasked with assigning a Hero to defeat the Demon Lord is an ambiguous figure known as the Prophet.</div>
    </div>
  </div>
</div>
<ol><li>VOLUME 01 Epub</li></ol>
</body></html>`

func TestParseNovelHTML_koboWidgetSynopsis(t *testing.T) {
	nd, err := ParseNovelHTML(novelPageKoboWidget, DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	want := "Tasked with assigning a Hero to defeat the Demon Lord is an ambiguous figure known as the Prophet."
	if nd.Description != want {
		t.Errorf("desc %q, want %q", nd.Description, want)
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
