package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─── constants ────────────────────────────────────────────────────────────────

const (
	rssURL     = "https://spillhistorie.no/feed/"
	podcastURL = "https://spillhistorie.no/category/podcast/feed/"
	kofiURL    = "https://ko-fi.com/joachimfroholt"
	mpvSock    = "/tmp/spillhistorie-tui.sock"
)

// ─── styles ───────────────────────────────────────────────────────────────────

var (
	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 2)

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#888888")).
				Padding(0, 2)

	listTitleStyle = lipgloss.NewStyle().
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

	playerBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#2D1B69")).
			Foreground(lipgloss.Color("#FAFAFA"))

	kofiStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF9B9B"))

	docStyle = lipgloss.NewStyle().Margin(1, 2)
)

// ─── app state ────────────────────────────────────────────────────────────────

type appState int

const (
	stateRSSLoading appState = iota
	stateBrowse
	stateArticleLoading
	stateArticle
	statePodcastLoading
	stateError
)

type activeTab int

const (
	tabArticles activeTab = iota
	tabPodcasts
)

// ─── data types ───────────────────────────────────────────────────────────────

type article struct {
	title     string
	link      string
	author    string
	published string
}

func (a article) Title() string       { return a.title }
func (a article) Description() string { return a.author + "  ·  " + a.published }
func (a article) FilterValue() string { return a.title + " " + a.author }

type podcastEpisode struct {
	title      string
	articleURL string
	author     string
	published  string
}

func (e podcastEpisode) Title() string       { return e.title }
func (e podcastEpisode) Description() string { return e.author + "  ·  " + e.published }
func (e podcastEpisode) FilterValue() string { return e.title }

// ─── player ───────────────────────────────────────────────────────────────────

type playerStatus int

const (
	playerStopped playerStatus = iota
	playerPlaying
	playerPaused
)

type player struct {
	cmd    *exec.Cmd
	status playerStatus
	title  string
	done   chan struct{}
}

func (p *player) isActive() bool { return p.status != playerStopped }

func (p *player) play(audioURL, title string) error {
	p.stop()
	cmd := exec.Command("mpv",
		"--no-video",
		"--no-terminal",
		"--quiet",
		"--input-ipc-server="+mpvSock,
		audioURL,
	)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	p.cmd = cmd
	p.title = title
	p.status = playerPlaying
	p.done = done
	go func() { cmd.Wait(); close(done) }()
	return nil
}

func (p *player) waitDone() tea.Cmd {
	done := p.done
	if done == nil {
		return nil
	}
	return func() tea.Msg { <-done; return playerDoneMsg{} }
}

func (p *player) togglePause() {
	if !p.isActive() {
		return
	}
	pausing := p.status == playerPlaying
	if pausing {
		p.status = playerPaused
	} else {
		p.status = playerPlaying
	}
	go func() {
		var conn net.Conn
		var err error
		for i := 0; i < 15; i++ {
			conn, err = net.Dial("unix", mpvSock)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			return
		}
		defer conn.Close()
		if pausing {
			fmt.Fprintf(conn, "{\"command\":[\"set_property\",\"pause\",true]}\n")
		} else {
			fmt.Fprintf(conn, "{\"command\":[\"set_property\",\"pause\",false]}\n")
		}
	}()
}

func (p *player) stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	if p.done != nil {
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
		p.done = nil
	}
	p.cmd = nil
	p.status = playerStopped
	p.title = ""
	os.Remove(mpvSock)
}

// ─── messages ─────────────────────────────────────────────────────────────────

type rssFetchedMsg []article
type podcastsFetchedMsg []podcastEpisode
type articleFetchedMsg struct{ title, content string }
type podcastAudioMsg struct{ audioURL, title string }
type playerDoneMsg struct{}
type errMsg struct{ err error }

// ─── model ────────────────────────────────────────────────────────────────────

type model struct {
	state           appState
	tab             activeTab
	articleList     list.Model
	podcastList     list.Model
	viewport        viewport.Model
	spinner         spinner.Model
	player          *player
	err             error
	width           int
	height          int
	podcastsLoaded  bool
	podcastsLoading bool
	currentArticle  article
}

func initialModel() model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4"))

	del := newDelegate()

	al := list.New([]list.Item{}, del, 0, 0)
	al.Title = "Artikler"
	al.Styles.Title = listTitleStyle
	al.SetShowStatusBar(true)
	al.SetFilteringEnabled(true)

	pl := list.New([]list.Item{}, del, 0, 0)
	pl.Title = "Podkast"
	pl.Styles.Title = listTitleStyle
	pl.SetShowStatusBar(true)
	pl.SetFilteringEnabled(true)

	return model{
		state:       stateRSSLoading,
		spinner:     s,
		articleList: al,
		podcastList: pl,
		player:      &player{},
	}
}

func newDelegate() list.DefaultDelegate {
	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.
		Foreground(lipgloss.Color("#C0A0FF")).
		BorderForeground(lipgloss.Color("#7D56F4"))
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.
		Foreground(lipgloss.Color("#9B88FF")).
		BorderForeground(lipgloss.Color("#7D56F4"))
	return d
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, fetchRSS)
}

// ─── update ───────────────────────────────────────────────────────────────────

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.player.stop()
			return m, tea.Quit

		case "q":
			if m.state == stateBrowse || m.state == stateError {
				m.player.stop()
				return m, tea.Quit
			}

		case "tab":
			if m.state == stateBrowse {
				if m.tab == tabArticles {
					m.tab = tabPodcasts
					if !m.podcastsLoaded && !m.podcastsLoading {
						m.podcastsLoading = true
						return m, tea.Batch(m.spinner.Tick, fetchPodcasts)
					}
				} else {
					m.tab = tabArticles
				}
			}

		case "esc":
			if m.state == stateArticle || m.state == stateArticleLoading {
				m.state = stateBrowse
				return m, nil
			}

		case "enter":
			if m.state == stateBrowse {
				switch m.tab {
				case tabArticles:
					if item, ok := m.articleList.SelectedItem().(article); ok {
						m.currentArticle = item
						m.state = stateArticleLoading
						return m, tea.Batch(m.spinner.Tick, fetchArticle(item.link, m.width))
					}
				case tabPodcasts:
					if item, ok := m.podcastList.SelectedItem().(podcastEpisode); ok {
						m.state = statePodcastLoading
						return m, tea.Batch(m.spinner.Tick, fetchPodcastAudio(item.articleURL, item.title))
					}
				}
			}

		case " ":
			if m.player.isActive() {
				m.player.togglePause()
				return m, nil
			}

		case "x":
			m.player.stop()
			return m, nil
		}

	case rssFetchedMsg:
		items := make([]list.Item, len(msg))
		for i, a := range msg {
			items[i] = a
		}
		m.articleList.SetItems(items)
		m.state = stateBrowse
		return m, nil

	case podcastsFetchedMsg:
		items := make([]list.Item, len(msg))
		for i, e := range msg {
			items[i] = e
		}
		m.podcastList.SetItems(items)
		m.podcastsLoaded = true
		m.podcastsLoading = false
		return m, nil

	case articleFetchedMsg:
		m.viewport.SetContent(msg.content)
		m.viewport.GotoTop()
		m.state = stateArticle
		return m, nil

	case podcastAudioMsg:
		if err := m.player.play(msg.audioURL, msg.title); err != nil {
			m.err = err
			m.state = stateError
			return m, nil
		}
		m.state = stateBrowse
		return m, m.player.waitDone()

	case playerDoneMsg:
		m.player.status = playerStopped
		m.player.title = ""
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
	case stateBrowse:
		if m.tab == tabArticles {
			m.articleList, cmd = m.articleList.Update(msg)
		} else {
			m.podcastList, cmd = m.podcastList.Update(msg)
		}
	case stateArticle:
		m.viewport, cmd = m.viewport.Update(msg)
	}
	return m, cmd
}

func (m *model) resize() {
	h, v := docStyle.GetFrameSize()
	playerH := 0
	if m.player.isActive() {
		playerH = 1
	}
	// tab bar (1) + kofi (1) + help (1) + spacing (2)
	listH := m.height - v - 5 - playerH
	if listH < 5 {
		listH = 5
	}
	m.articleList.SetSize(m.width-h, listH)
	m.podcastList.SetSize(m.width-h, listH)

	// article title (1) + meta (1) + 2×sep (2) + help (1) + kofi (1) + spacing (3)
	vpH := m.height - v - 9 - playerH
	if vpH < 5 {
		vpH = 5
	}
	m.viewport = viewport.New(m.width-h, vpH)
}

// ─── view ─────────────────────────────────────────────────────────────────────

func (m model) View() string {
	if m.width == 0 {
		return ""
	}
	switch m.state {
	case stateRSSLoading:
		return "\n\n  " + m.spinner.View() + " Laster Spillhistorie.no…\n\n"
	case stateArticleLoading:
		return "\n\n  " + m.spinner.View() + " Henter artikkel…\n\n"
	case statePodcastLoading:
		return "\n\n  " + m.spinner.View() + " Kobler til podcast…\n\n"
	case stateBrowse:
		return m.browseView()
	case stateArticle:
		return m.articleView()
	case stateError:
		return docStyle.Render(
			errorStyle.Render("Feil: "+m.err.Error()) +
				"\n\nTrykk q for å avslutte.",
		)
	}
	return ""
}

func (m model) browseView() string {
	var body string
	if m.tab == tabArticles {
		body = m.articleList.View()
	} else if m.podcastsLoading {
		body = "\n  " + m.spinner.View() + " Laster podcaster…\n"
	} else {
		body = m.podcastList.View()
	}

	footer := kofiStyle.Render("♥ Støtt spillhistorie.no: "+kofiURL) + "\n" +
		helpStyle.Render("tab: bytt visning  ·  enter: åpne  ·  /: søk  ·  q: avslutt")

	parts := []string{m.tabBar(), body, "", footer}
	if m.player.isActive() {
		parts = append(parts, "", m.playerBar())
	}
	return docStyle.Render(strings.Join(parts, "\n"))
}

func (m model) articleView() string {
	sep := strings.Repeat("─", max(0, m.width-4))
	lines := []string{
		articleTitleStyle.Render(m.currentArticle.title),
		metaStyle.Render(m.currentArticle.author + "  ·  " + m.currentArticle.published),
		sep,
		m.viewport.View(),
		sep,
		helpStyle.Render("↑/↓ j/k: scroll  ·  g/G: topp/bunn  ·  esc: tilbake  ·  q: avslutt"),
		kofiStyle.Render("♥ Støtt spillhistorie.no: " + kofiURL),
	}
	if m.player.isActive() {
		lines = append(lines, "", m.playerBar())
	}
	return docStyle.Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

func (m model) tabBar() string {
	var art, pod string
	if m.tab == tabArticles {
		art = activeTabStyle.Render("Artikler")
		pod = inactiveTabStyle.Render("Podkast")
	} else {
		art = inactiveTabStyle.Render("Artikler")
		pod = activeTabStyle.Render("Podkast")
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, art, pod)
}

func (m model) playerBar() string {
	icon, verb := "▶", "Spiller"
	if m.player.status == playerPaused {
		icon, verb = "⏸", "Pause"
	}
	title := clamp(m.player.title, m.width-44)
	bar := fmt.Sprintf(" %s %s: %s   [space: pause  x: stopp]", icon, verb, title)
	return playerBarStyle.Width(m.width - 4).Render(bar)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// clamp truncates s to n visible runes, adding … if needed.
func clamp(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// stripTags removes HTML tags from a string.
func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// trunc truncates s to n runes.
func trunc(s string, n int) string {
	r := []rune(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ─── main ────────────────────────────────────────────────────────────────────

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Feil: %v\n", err)
		os.Exit(1)
	}
}
