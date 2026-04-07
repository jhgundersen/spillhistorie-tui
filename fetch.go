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

// quizTagIDs are the WP tag IDs that mark quiz articles.
var quizTagIDs = map[int]bool{300: true, 6750: true}

// fetchArticleList is Phase 1: fast metadata-only fetch (~0.5s).
// Returns titles, links, dates and tag IDs so the list can be shown immediately.
func fetchArticleList(cat articleCategory) tea.Cmd {
	return func() tea.Msg {
		u := wpAPIBase + "?_fields=id,link,date,title,tags&per_page=10"
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
			ID   int    `json:"id"`
			Link string `json:"link"`
			Date string `json:"date"`
			Tags []int  `json:"tags"`
			Title struct {
				Rendered string `json:"rendered"`
			} `json:"title"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&posts); err != nil {
			return errMsg{err}
		}

		articles := make([]article, 0, len(posts))
		for _, p := range posts {
			published := ""
			if t, err := time.Parse("2006-01-02T15:04:05", p.Date); err == nil {
				published = t.Format("02 Jan 2006")
			}
			articles = append(articles, article{
				id:        p.ID,
				title:     stripTags(p.Title.Rendered),
				link:      p.Link,
				published: published,
				tagIDs:    p.Tags,
			})
		}
		return articlesFetchedMsg(articles)
	}
}

// fetchArticleContent is Phase 2: background fetch of content + author + image.
// Fires after the list is shown; pre-loads everything needed for instant article opens.
func fetchArticleContent(categoryIdx int, ids []int) tea.Cmd {
	return func() tea.Msg {
		if len(ids) == 0 {
			return nil
		}
		idStrs := make([]string, len(ids))
		for i, id := range ids {
			idStrs[i] = strconv.Itoa(id)
		}
		u := wpAPIBase + "?_fields=id,link,content,_links&_embed=author,wp:featuredmedia&per_page=10&include=" + strings.Join(idStrs, ",")
		resp, err := http.Get(u)
		if err != nil {
			return nil // best-effort; list is already shown
		}
		defer resp.Body.Close()

		var posts []struct {
			ID   int    `json:"id"`
			Link string `json:"link"`
			Content struct {
				Rendered string `json:"rendered"`
			} `json:"content"`
			Embedded struct {
				Author []struct {
					Name string `json:"name"`
				} `json:"author"`
				FeaturedMedia []struct {
					SourceURL string `json:"source_url"`
				} `json:"wp:featuredmedia"`
			} `json:"_embedded"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&posts); err != nil {
			return nil
		}

		byID := make(map[int]articleEnrichment, len(posts))
		for _, p := range posts {
			e := articleEnrichment{content: p.Content.Rendered}
			if len(p.Embedded.Author) > 0 {
				e.author = p.Embedded.Author[0].Name
			}
			if len(p.Embedded.FeaturedMedia) > 0 {
				e.featuredImage = p.Embedded.FeaturedMedia[0].SourceURL
			}
			byID[p.ID] = e
		}
		return articlesContentMsg{categoryIdx: categoryIdx, byID: byID}
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

// isQuizArticle returns true when the article has a quiz tag (IDs 300 or 6750).
func isQuizArticle(tagIDs []int) bool {
	for _, id := range tagIDs {
		if quizTagIDs[id] {
			return true
		}
	}
	return false
}

func fetchArticle(a article) tea.Cmd {
	return func() tea.Msg {
		var rawHTML, heroImage string

		if a.content != "" {
			// Content already loaded from WP API — no HTTP fetch needed.
			rawHTML = a.content
			heroImage = a.featuredImage
		} else {
			// Fallback: fetch the page and extract with readability.
			resp, err := http.Get(a.link)
			if err != nil {
				return errMsg{err}
			}
			defer resp.Body.Close()

			parsedURL, err := url.ParseRequestURI(a.link)
			if err != nil {
				return errMsg{err}
			}

			pageBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return errMsg{err}
			}

			parsed, err := readability.FromReader(bytes.NewReader(pageBytes), parsedURL)
			if err != nil {
				return errMsg{err}
			}
			rawHTML = parsed.Content
			heroImage = parsed.Image
		}

		// Quiz articles show images inline; all others use the gallery.
		var images []ImageRef
		var inlineImgs []ImageRef
		if isQuizArticle(a.tagIDs) {
			inlineImgs = ExtractInlineImages(rawHTML)
		} else {
			images = ExtractArticleImages(rawHTML)
		}

		return articleFetchedMsg{
			title:      a.title,
			rawHTML:    rawHTML,
			imageURL:   heroImage,
			images:     images,
			inlineImgs: inlineImgs,
		}
	}
}
