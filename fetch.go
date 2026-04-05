package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"

	tea "github.com/charmbracelet/bubbletea"
	readability "github.com/go-shiori/go-readability"
	"github.com/mmcdole/gofeed"
)

// ─── RSS ─────────────────────────────────────────────────────────────────────

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

func fetchPodcasts() tea.Msg {
	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(podcastURL)
	if err != nil {
		return errMsg{err}
	}
	episodes := make([]podcastEpisode, 0, len(feed.Items))
	for _, item := range feed.Items {
		author := ""
		if item.Author != nil {
			author = item.Author.Name
		}
		published := ""
		if item.PublishedParsed != nil {
			published = item.PublishedParsed.Format("02 Jan 2006")
		}
		episodes = append(episodes, podcastEpisode{
			title:      item.Title,
			articleURL: item.Link,
			author:     author,
			published:  published,
		})
	}
	return podcastsFetchedMsg(episodes)
}

// ─── article ─────────────────────────────────────────────────────────────────

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

// ─── podcast audio ───────────────────────────────────────────────────────────

var (
	transistorRe = regexp.MustCompile(`share\.transistor\.fm/e/([a-z0-9]+)`)
	audioURLRe   = regexp.MustCompile(`"trackable_media_url":"(https://[^"]+)"`)
)

func fetchPodcastAudio(articleURL, title string) tea.Cmd {
	return func() tea.Msg {
		// Step 1: fetch the spillhistorie.no article page to find the embed
		resp, err := http.Get(articleURL)
		if err != nil {
			return errMsg{err}
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return errMsg{err}
		}

		// Step 2: extract Transistor.fm embed ID
		m := transistorRe.FindSubmatch(body)
		if len(m) < 2 {
			return errMsg{fmt.Errorf("ingen podcast-avspiller funnet på denne siden")}
		}
		embedID := string(m[1])

		// Step 3: fetch Transistor.fm embed page which contains the MP3 URL in JSON
		resp2, err := http.Get("https://share.transistor.fm/e/" + embedID)
		if err != nil {
			return errMsg{err}
		}
		body2, err := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if err != nil {
			return errMsg{err}
		}

		m2 := audioURLRe.FindSubmatch(body2)
		if len(m2) < 2 {
			return errMsg{fmt.Errorf("lydlenke ikke funnet i Transistor.fm")}
		}

		return podcastAudioMsg{
			audioURL: string(m2[1]),
			title:    title,
		}
	}
}
