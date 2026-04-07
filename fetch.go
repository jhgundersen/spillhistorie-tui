package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	readability "github.com/go-shiori/go-readability"
	"github.com/mmcdole/gofeed"
)

// ─── articles JSON API ────────────────────────────────────────────────────────

const wpAPIBase = "https://www.spillhistorie.no/wp-json/wp/v2/posts"

func fetchArticles(cat articleCategory) tea.Cmd {
	return func() tea.Msg {
		u := wpAPIBase + "?_embed=1&per_page=20"
		if cat.categoryID > 0 {
			u += fmt.Sprintf("&categories=%d", cat.categoryID)
		}
		if len(cat.tagIDs) > 0 {
			parts := make([]string, len(cat.tagIDs))
			for i, id := range cat.tagIDs {
				parts[i] = strconv.Itoa(id)
			}
			u += "&tags=" + strings.Join(parts, ",")
		}
		resp, err := http.Get(u)
		if err != nil {
			return errMsg{err}
		}
		defer resp.Body.Close()

		var posts []struct {
			Link  string `json:"link"`
			Date  string `json:"date"`
			Title struct {
				Rendered string `json:"rendered"`
			} `json:"title"`
			Embedded struct {
				Author []struct {
					Name string `json:"name"`
				} `json:"author"`
				Terms [][]struct {
					Name string `json:"name"`
				} `json:"wp:term"`
			} `json:"_embedded"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&posts); err != nil {
			return errMsg{err}
		}

		articles := make([]article, 0, len(posts))
		for _, p := range posts {
			author := ""
			if len(p.Embedded.Author) > 0 {
				author = p.Embedded.Author[0].Name
			}
			published := ""
			if t, err := time.Parse("2006-01-02T15:04:05", p.Date); err == nil {
				published = t.Format("02 Jan 2006")
			}
			var cats []string
			if len(p.Embedded.Terms) > 0 {
				for _, term := range p.Embedded.Terms[0] {
					cats = append(cats, term.Name)
				}
			}
			articles = append(articles, article{
				title:      stripTags(p.Title.Rendered),
				link:       p.Link,
				author:     author,
				published:  published,
				categories: cats,
			})
		}
		return articlesFetchedMsg(articles)
	}
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

// isQuizArticle returns true when the article's categories indicate it is
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

		// Quiz articles (tagged "quiz" or "fredagsquiz" in RSS) show images
		// inline at their natural positions. All other articles use the gallery.
		var images []ImageRef
		var inlineImgs []ImageRef
		if isQuizArticle(a.categories) {
			inlineImgs = ExtractInlineImages(parsed.Content)
		} else {
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
