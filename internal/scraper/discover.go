package scraper

import (
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Discover (#91) surfaces new light novels to read from jnovels, separate from
// the library-only Randomizer. Two feeds: Latest (the newest epub releases) and
// Find-new (jnovels' own /randomizer/). Both hand back Release picks; the server
// caches them and resolves the spoiler-safe series page on demand.

// Release is one jnovels discovery pick: the post URL, its cover, and a display
// title. Latest carries the real post title; Find-new derives the title from the
// URL slug (the /randomizer/ grid ships no title text, only cover + link).
type Release struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	CoverURL string `json:"cover_url"`
}

// maxLatestPages caps how many listing pages LatestEpubReleases walks before it
// gives up — a safety net so a thin catalog (or a layout change that stops the
// parser matching) can't spin through every page on the site.
const maxLatestPages = 8

// LatestEpubReleases returns up to n most-recent jnovels epub releases, walking
// the front page then /page/2/, /page/3/, … (jnovels lists ~6 novel posts per
// page and duplicates each for pdf/epub) until n epub releases are gathered, a
// page yields no new picks, or maxLatestPages is hit. Deduped by URL across
// pages.
func (c *Client) LatestEpubReleases(n int) ([]Release, error) {
	if n <= 0 {
		return nil, nil
	}
	var out []Release
	seen := map[string]bool{}
	for page := 1; page <= maxLatestPages && len(out) < n; page++ {
		url := jnovelsBaseURL + "/"
		if page > 1 {
			url = fmt.Sprintf("%s/page/%d/", jnovelsBaseURL, page)
		}
		doc, err := c.fetch(url)
		if err != nil {
			// Page 1 failing is a real error; a later page 404 just means we've
			// run off the end of the listing, so stop with what we have.
			if page == 1 {
				return nil, err
			}
			break
		}
		added := 0
		for _, r := range parseLatestReleases(doc) {
			if seen[r.URL] {
				continue
			}
			seen[r.URL] = true
			out = append(out, r)
			added++
			if len(out) >= n {
				break
			}
		}
		if added == 0 {
			break
		}
	}
	return out, nil
}

// ParseLatestHTML ranks epub releases from raw jnovels listing HTML (tests +
// live-test), mirroring ParseNovelHTML.
func ParseLatestHTML(html string) ([]Release, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	return parseLatestReleases(doc), nil
}

// parseLatestReleases reads the epub novel posts off a jnovels listing page. A
// post qualifies when its <article> is tagged category-light-novels (skips the
// site's own app-promo posts) and its title is an epub release (skips the pdf
// twin jnovels publishes for every volume). The cover comes from the post's
// featured-media image; page order is preserved (newest first).
func parseLatestReleases(doc *goquery.Document) []Release {
	var out []Release
	doc.Find("article.category-light-novels").Each(func(_ int, art *goquery.Selection) {
		a := art.Find("h1.post-title.entry-title > a[href]").First()
		title := strings.TrimSpace(a.Text())
		href, _ := a.Attr("href")
		href = strings.TrimSpace(href)
		if title == "" || href == "" {
			return
		}
		if formatRank(title) != 0 { // epub only
			return
		}
		cover, _ := art.Find("div.featured-media img").First().Attr("src")
		out = append(out, Release{
			Title:    cleanReleaseTitle(title),
			URL:      href,
			CoverURL: strings.TrimSpace(cover),
		})
	})
	return out
}

// RandomizerPicks returns the picks jnovels' /randomizer/ page renders (20 random
// posts, a mix of epub and pdf) in a single request. Each pick carries its cover
// and post URL; the title is derived from the slug since the grid ships no title
// text.
func (c *Client) RandomizerPicks() ([]Release, error) {
	doc, err := c.fetch(jnovelsBaseURL + "/randomizer/")
	if err != nil {
		return nil, err
	}
	return parseRandomizer(doc), nil
}

// ParseRandomizerHTML parses picks from raw /randomizer/ HTML (tests + live-test).
func ParseRandomizerHTML(html string) ([]Release, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	return parseRandomizer(doc), nil
}

// parseRandomizer reads the /randomizer/ listing: each pick is a
// div.listing-item holding an <a class="image"> to the post and its cover <img>.
// Deduped by URL; title inferred from the slug.
func parseRandomizer(doc *goquery.Document) []Release {
	var out []Release
	seen := map[string]bool{}
	doc.Find("div.listing-item a.image[href]").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" || seen[href] {
			return
		}
		seen[href] = true
		cover, _ := a.Find("img").First().Attr("src")
		out = append(out, Release{
			Title:    TitleFromSlug(href),
			URL:      href,
			CoverURL: strings.TrimSpace(cover),
		})
	})
	return out
}

// OriginalPost follows a jnovels volume page's "Refer to original post" link to
// the series aggregate page (the ...-light-novel-epub/ post that carries the
// whole volume list — the page the add flow scrapes). Returns an error if the
// link isn't found.
func (c *Client) OriginalPost(volumeURL string) (string, error) {
	doc, err := c.fetch(volumeURL)
	if err != nil {
		return "", err
	}
	if u := parseOriginalPost(doc); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no original-post link found on %s", volumeURL)
}

// ParseOriginalPostHTML extracts the original-post link from raw volume HTML
// (tests + live-test).
func ParseOriginalPostHTML(html string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}
	return parseOriginalPost(doc), nil
}

// parseOriginalPost finds the anchor whose text is "Refer to original post" (the
// link back to the series page) and returns its href, or "" if absent.
func parseOriginalPost(doc *goquery.Document) string {
	var out string
	doc.Find("a[href]").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		if strings.EqualFold(strings.TrimSpace(a.Text()), "Refer to original post") {
			out = strings.TrimSpace(a.AttrOr("href", ""))
			return out == ""
		}
		return true
	})
	return out
}

// cleanReleaseTitle strips the trailing " Epub"/" Pdf" format word jnovels
// appends to every post title, leaving the human-readable volume title.
func cleanReleaseTitle(title string) string {
	for _, suffix := range []string{" Epub", " Pdf", " EPUB", " PDF"} {
		if strings.HasSuffix(title, suffix) {
			return strings.TrimSpace(title[:len(title)-len(suffix)])
		}
	}
	return title
}

// TitleFromSlug turns a jnovels post URL into a display title: it takes the last
// path segment, drops the trailing epub/pdf/light-novel format markers, and
// title-cases the remaining words. Used for /randomizer/ picks, whose grid ships
// no title text.
func TitleFromSlug(postURL string) string {
	slug := strings.Trim(postURL, "/")
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}
	words := strings.Split(slug, "-")
	// Drop trailing format markers ("... light novel epub", "... volume 10 pdf").
	for len(words) > 0 {
		last := strings.ToLower(words[len(words)-1])
		if last == "epub" || last == "pdf" || last == "novel" || last == "light" {
			words = words[:len(words)-1]
			continue
		}
		break
	}
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
