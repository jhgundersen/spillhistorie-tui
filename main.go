package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	readability "github.com/go-shiori/go-readability"
	"github.com/mmcdole/gofeed"
)

const rssURL = "https://spillhistorie.no/feed/"

var (
	appTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1)

	articleTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#C0A0FF"))

	metaStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555"))

	docStyle = lipgloss.NewStyle().Margin(1, 2)
)

// ---------- app state ----------

type appState int

const (
	stateLoading appState = iota
	stateList
	stateArticleLoading
	stateArticle
	stateError
)

// ---------- data types ----------

type article struct {
	title     string
	link      string
	author    string
	published string
	excerpt   string
}

func (a article) Title() string       { return a.title }
func (a article) Description() string { return fmt.Sprintf("%s  ·  %s", a.author, a.published) }
func (a article) FilterValue() string { return a.title + " " + a.author }

// ---------- messages ----------

type rssFetchedMsg []article

type articleFetchedMsg struct {
	title   string
	content string
}

type errMsg struct{ err error }

func (e errMsg) Error() string { return e.err.Error() }

// ---------- commands ----------

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
		excerpt := stripTags(item.Description)
		if utf8.RuneCountInString(excerpt) > 110 {
			runes := []rune(excerpt)
			excerpt = string(runes[:110]) + "…"
		}
		articles = append(articles, article{
			title:     item.Title,
			link:      item.Link,
			author:    author,
			published: published,
			excerpt:   excerpt,
		})
	}
	return rssFetchedMsg(articles)
}

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

		converter := htmltomd.NewConverter("", true, nil)
		markdown, err := converter.ConvertString(parsed.Content)
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

		renderer, err := glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(contentWidth),
		)
		if err != nil {
			return errMsg{err}
		}
		rendered, err := renderer.Render(markdown)
		if err != nil {
			return errMsg{err}
		}

		return articleFetchedMsg{
			title:   parsed.Title,
			content: rendered,
		}
	}
}

// ---------- model ----------

type model struct {
	state          appState
	list           list.Model
	viewport       viewport.Model
	spinner        spinner.Model
	err            error
	width          int
	height         int
	currentArticle article
}

func initialModel() model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4"))

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(lipgloss.Color("#C0A0FF")).
		BorderForeground(lipgloss.Color("#7D56F4"))
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(lipgloss.Color("#9B88FF")).
		BorderForeground(lipgloss.Color("#7D56F4"))

	l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Spillhistorie.no"
	l.Styles.Title = appTitleStyle
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)

	return model{
		state:   stateLoading,
		spinner: s,
		list:    l,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, fetchRSS)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v)
		m.viewport = viewport.New(msg.Width-h, msg.Height-v-7)

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			if m.state == stateList || m.state == stateError {
				return m, tea.Quit
			}
		case "esc":
			if m.state == stateArticle || m.state == stateArticleLoading {
				m.state = stateList
				return m, nil
			}
		case "enter":
			if m.state == stateList {
				if item, ok := m.list.SelectedItem().(article); ok {
					m.currentArticle = item
					m.state = stateArticleLoading
					return m, tea.Batch(m.spinner.Tick, fetchArticle(item.link, m.width))
				}
			}
		}

	case rssFetchedMsg:
		items := make([]list.Item, len(msg))
		for i, a := range msg {
			items[i] = a
		}
		m.list.SetItems(items)
		m.state = stateList
		return m, nil

	case articleFetchedMsg:
		m.viewport.SetContent(msg.content)
		m.viewport.GotoTop()
		m.state = stateArticle
		return m, nil

	case errMsg:
		m.err = msg.err
		m.state = stateError
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	switch m.state {
	case stateList:
		m.list, cmd = m.list.Update(msg)
	case stateArticle:
		m.viewport, cmd = m.viewport.Update(msg)
	}

	return m, cmd
}

func (m model) View() string {
	switch m.state {
	case stateLoading:
		return "\n\n  " + m.spinner.View() + " Henter artikler fra Spillhistorie.no…\n\n"

	case stateList:
		return docStyle.Render(m.list.View())

	case stateArticleLoading:
		return "\n\n  " + m.spinner.View() + " Henter artikkel…\n\n"

	case stateArticle:
		sep := strings.Repeat("─", max(0, m.width-4))
		help := helpStyle.Render("↑/↓ j/k: scroll  •  g/G: topp/bunn  •  esc: tilbake  •  q: avslutt")
		return docStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			articleTitleStyle.Render(m.currentArticle.title),
			metaStyle.Render(fmt.Sprintf("%s  ·  %s", m.currentArticle.author, m.currentArticle.published)),
			sep,
			m.viewport.View(),
			sep,
			help,
		))

	case stateError:
		return docStyle.Render(
			errorStyle.Render(fmt.Sprintf("Feil: %v", m.err))+
				"\n\nTrykk q for å avslutte.",
		)
	}
	return ""
}

// ---------- helpers ----------

func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------- main ----------

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Feil: %v\n", err)
		os.Exit(1)
	}
}
