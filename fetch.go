package main

import (
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
			title:     item.Title,
			link:      item.Link,
			author:    author,
			published: published,
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

func fetchArticle(articleURL string, width int) tea.Cmd {
	return func() tea.Msg {
		resp, err := http.Get(articleURL)
		if err != nil {
			return errMsg{err}
		}
		defer resp.Body.Close()

		parsedURL, err := url.ParseRequestURI(articleURL)
		if err != nil {
			return errMsg{err}
		}

		parsed, err := readability.FromReader(resp.Body, parsedURL)
		if err != nil {
			return errMsg{err}
		}

		contentWidth := width - 6
		if contentWidth < 40 {
			contentWidth = 40
		}
		if contentWidth > 120 {
			contentWidth = 120
		}

		return articleFetchedMsg{
			title:   parsed.Title,
			content: renderHTML(parsed.Content, contentWidth),
		}
	}
}
