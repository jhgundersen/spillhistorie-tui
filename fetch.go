package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	readability "github.com/go-shiori/go-readability"
	"github.com/mmcdole/gofeed"
)

// ─── articles RSS ─────────────────────────────────────────────────────────────

func fetchRSS() tea.Msg {
	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(rssURL)
	if err != nil {
		return errMsg{err}
	}
	articles := make([]article, 0, len(feed.Items))
	for _, item := range feed.Items {
		author := ""
		if item.Author != nil {
			author = item.Author.Name
		}
		published := ""
		if item.PublishedParsed != nil {
			published = item.PublishedParsed.Format("02 Jan 2006")
		}
		articles = append(articles, article{
			title:      item.Title,
			link:       item.Link,
			author:     author,
			published:  published,
			categories: item.Categories,
		})
	}
	return rssFetchedMsg(articles)
}

// ─── podcast feeds ────────────────────────────────────────────────────────────

// fetchPodcasts fetches all configured podcast RSS feeds in parallel and
// returns a merged, date-sorted list of episodes.
func fetchPodcasts() tea.Msg {
	type result struct {
		eps []podcastEpisode
		err error
	}

	chs := make([]chan result, len(podcastFeeds))
	for i, feed := range podcastFeeds {
		chs[i] = make(chan result, 1)
		go func(name, u string, ch chan result) {
			eps, err := parsePodcastFeed(name, u)
			ch <- result{eps, err}
		}(feed.name, feed.url, chs[i])
	}

	var all []podcastEpisode
	for _, ch := range chs {
		r := <-ch
		if r.err == nil {
			all = append(all, r.eps...)
		}
	}

	// Sort newest-first by publish time
	sort.Slice(all, func(i, j int) bool {
		return all[i].publishedUnix > all[j].publishedUnix
	})

	return podcastsFetchedMsg(all)
}

// parsePodcastFeed parses a single RSS feed and returns episode list.
func parsePodcastFeed(seriesName, feedURL string) ([]podcastEpisode, error) {
	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(feedURL)
	if err != nil {
		return nil, err
	}

	eps := make([]podcastEpisode, 0, len(feed.Items))
	for _, item := range feed.Items {
		// Audio URL from enclosure
		audioURL := ""
		if len(item.Enclosures) > 0 {
			audioURL = item.Enclosures[0].URL
		}
		if audioURL == "" {
			continue // skip items without audio
		}

		// Duration in seconds
		duration := 0
		if ext, ok := item.Extensions["itunes"]["duration"]; ok && len(ext) > 0 {
			duration = parseDuration(ext[0].Value)
		}

		author := ""
		if item.Author != nil {
			author = item.Author.Name
		}
		if author == "" && feed.Author != nil {
			author = feed.Author.Name
		}

		published := ""
		publishedUnix := int64(0)
		if item.PublishedParsed != nil {
			published = item.PublishedParsed.Format("02 Jan 2006")
			publishedUnix = item.PublishedParsed.Unix()
		}

		eps = append(eps, podcastEpisode{
			title:         item.Title,
			audioURL:      audioURL,
			duration:      duration,
			series:        seriesName,
			author:        author,
			published:     published,
			publishedUnix: publishedUnix,
		})
	}
	return eps, nil
}

// parseDuration parses an itunes:duration value, which can be either:
//   - plain seconds: "2759"
//   - HH:MM:SS or MM:SS: "46:07" or "1:12:34"
func parseDuration(s string) int {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, ":") {
		n, _ := strconv.Atoi(s)
		return n
	}
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		n, _ := strconv.Atoi(strings.TrimSpace(p))
		total = total*60 + n
	}
	return total
}

// ─── article reader ───────────────────────────────────────────────────────────

// isQuizArticle returns true when the article's RSS categories indicate it is
// a quiz (contains "quiz" or "fredagsquiz", case-insensitive).
func isQuizArticle(categories []string) bool {
	for _, c := range categories {
		lc := strings.ToLower(c)
		if lc == "quiz" || lc == "fredagsquiz" {
			return true
		}
	}
	return false
}

func fetchArticle(a article) tea.Cmd {
	return func() tea.Msg {
		resp, err := http.Get(a.link)
		if err != nil {
			return errMsg{err}
		}
		defer resp.Body.Close()

		parsedURL, err := url.ParseRequestURI(a.link)
		if err != nil {
			return errMsg{err}
		}

		// Read the full page once so we can pass it to both readability
		// and our image extractor. Readability may strip image-heavy blocks
		// (e.g. gallery/quiz articles), so we extract images from the raw HTML.
		pageBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return errMsg{err}
		}

		parsed, err := readability.FromReader(bytes.NewReader(pageBytes), parsedURL)
		if err != nil {
			return errMsg{err}
		}

		contentImages := ExtractArticleImages(parsed.Content)
		bodyImages := ExtractPageBodyImages(string(pageBytes))

		// Quiz articles (tagged "quiz" or "fredagsquiz" in RSS) show images
		// inline at their natural positions. All other articles use the gallery.
		// Fall back to raw body images if readability stripped most of them.
		var images []ImageRef
		var inlineImgs []ImageRef
		switch {
		case isQuizArticle(a.categories):
			inlineImgs = ExtractInlineImages(parsed.Content)
		case len(bodyImages) >= 3 && len(bodyImages) > len(contentImages)+2:
			inlineImgs = bodyImages
		default:
			images = contentImages
		}

		return articleFetchedMsg{
			title:      parsed.Title,
			rawHTML:    parsed.Content,
			imageURL:   parsed.Image,
			images:     images,
			inlineImgs: inlineImgs,
		}
	}
}
