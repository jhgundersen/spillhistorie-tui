package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─── constants ────────────────────────────────────────────────────────────────

// version is set at build time via -ldflags "-X main.version=v1.2.3".
var version = "dev"

const (
	rssURL  = "https://spillhistorie.no/feed/"
	kofiURL = "https://ko-fi.com/joachimfroholt"
	mpvSock = "/tmp/spillhistorie.sock"
)

var podcastFeeds = []struct{ name, url string }{
	{"Diskettkameratene", "https://feeds.transistor.fm/diskettkameratene"},
	{"cd SPILL", "https://feed.podbean.com/cdspill/feed.xml"},
}

// ─── styles ───────────────────────────────────────────────────────────────────

var (
	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("5")).
			Padding(0, 2)

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8")).
				Padding(0, 2)

	listTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("5")).
			Padding(0, 1)

	articleTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("13")) // bright magenta

	metaStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9")) // bright red

	playerBGStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("0")).
			Foreground(lipgloss.Color("7"))

	playerTitleStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("0")).
				Foreground(lipgloss.Color("13")).
				Bold(true)

	playerMetaStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("0")).
			Foreground(lipgloss.Color("8"))

	progressFilledStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("5"))

	progressCursorStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("13"))

	progressEmptyStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8"))

	kofiStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	docStyle = lipgloss.NewStyle().Margin(1, 2)
)

// ─── app state ────────────────────────────────────────────────────────────────

type appState int

const (
	stateRSSLoading appState = iota
	stateBrowse
	stateArticleLoading
	stateArticle
	stateImageView
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
	title         string
	audioURL      string
	duration      int // seconds
	series        string
	author        string
	published     string
	publishedUnix int64
}

func (e podcastEpisode) Title() string { return e.title }
func (e podcastEpisode) Description() string {
	return fmt.Sprintf("%s  ·  %s  ·  %s", e.series, e.published, fmtDuration(float64(e.duration)))
}
func (e podcastEpisode) FilterValue() string { return e.title + " " + e.series }

// ─── player ───────────────────────────────────────────────────────────────────

type playerStatus int

const (
	playerStopped playerStatus = iota
	playerPlaying
	playerPaused
)

type player struct {
	cmd      *exec.Cmd
	status   playerStatus
	audioURL string
	title    string
	series   string
	done     chan struct{}
	pos      float64 // protected by posSync; use getPos/setPos
	posSync  sync.Mutex
	duration float64 // seconds, from RSS
}

func (p *player) getPos() float64 {
	p.posSync.Lock()
	defer p.posSync.Unlock()
	return p.pos
}

func (p *player) setPos(v float64) {
	p.posSync.Lock()
	p.pos = v
	p.posSync.Unlock()
}

func (p *player) isActive() bool { return p.status != playerStopped }

func (p *player) play(audioURL, title, series string, duration, startPos float64) error {
	p.stop()
	args := []string{
		"--no-video", "--no-terminal", "--quiet",
		"--input-ipc-server=" + mpvSock,
	}
	if startPos > 0 {
		args = append(args, fmt.Sprintf("--start=%g", startPos))
	}
	args = append(args, audioURL)
	cmd := exec.Command("mpv", args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	p.cmd = cmd
	p.audioURL = audioURL
	p.title = title
	p.series = series
	p.duration = duration
	p.setPos(startPos)
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
		for i := 0; i < 20; i++ {
			conn, err = net.DialTimeout("unix", mpvSock, 100*time.Millisecond)
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

// stop halts playback and saves position for later resuming.
func (p *player) stop() {
	pos := p.getPos()
	if p.isActive() && pos > 10 && (p.duration <= 0 || pos < p.duration-30) {
		saveResume(&resumeState{
			AudioURL: p.audioURL,
			Title:    p.title,
			Series:   p.series,
			Duration: p.duration,
			Pos:      pos,
		})
	}
	p.kill()
}

// finish marks the episode as completed and removes any resume state.
func (p *player) finish() {
	deleteResume()
	p.kill()
}

func (p *player) kill() {
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
	p.audioURL = ""
	p.title = ""
	p.series = ""
	p.setPos(0)
	p.duration = 0
	os.Remove(mpvSock)
}

// seek sends a relative seek command to mpv (delta in seconds).
func (p *player) seek(delta float64) {
	if !p.isActive() {
		return
	}
	go func() {
		conn, err := net.DialTimeout("unix", mpvSock, 100*time.Millisecond)
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "{\"command\":[\"seek\",%g,\"relative\"]}\n", delta)
	}()
}

// ─── messages ─────────────────────────────────────────────────────────────────

type rssFetchedMsg []article
type podcastsFetchedMsg []podcastEpisode
type articleFetchedMsg struct {
	title      string
	rawHTML    string
	imageURL   string
	images     []ImageRef // gallery images (regular articles)
	inlineImgs []ImageRef // inline images (quiz/visual articles)
}
type playerDoneMsg struct{}
type playerPosMsg struct{ pos float64 } // pos < 0 means query failed
type bodyImageFetchedMsg struct{ rendered string }
type inlineImageLoadedMsg struct {
	idx      int
	rendered string
}
type errMsg struct{ err error }

// ─── model ────────────────────────────────────────────────────────────────────

type model struct {
	state               appState
	tab                 activeTab
	articleList         list.Model
	podcastList         list.Model
	viewport            viewport.Model
	spinner             spinner.Model
	player              *player
	err                 error
	width               int
	height              int
	podcastsLoaded      bool
	podcastsLoading     bool
	currentArticle      article
	currentHTML         string
	currentArticleImage string
	articleImages       []ImageRef         // gallery images (regular articles)
	inlineImgs          []ImageRef         // inline images (quiz/visual articles)
	inlineImgCache      map[int]string     // idx → rendered chafa output
	imageViewIdx        int                // next image index to show (cycles)
	imageViewSrc        string             // image currently being shown/loaded
	imageViewAlt        string
	imageRendered       string             // empty = still loading
}

func initialModel() model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))

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
		Foreground(lipgloss.Color("13")).
		BorderForeground(lipgloss.Color("5"))
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.
		Foreground(lipgloss.Color("5")).
		BorderForeground(lipgloss.Color("5"))
	return d
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick, fetchRSS}
	if rs := loadResume(); rs != nil {
		if err := m.player.play(rs.AudioURL, rs.Title, rs.Series, rs.Duration, rs.Pos); err == nil {
			cmds = append(cmds, m.player.waitDone(), pollPlayerPos())
		}
	}
	return tea.Batch(cmds...)
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
			m.player.stop()
			return m, tea.Quit

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

		case "g":
			if m.state == stateArticle {
				m.viewport.GotoTop()
				return m, nil
			}

		case "G":
			if m.state == stateArticle {
				m.viewport.GotoBottom()
				return m, nil
			}

		case "i":
			if (m.state == stateArticle || m.state == stateImageView) && len(m.articleImages) > 0 {
				img := m.articleImages[m.imageViewIdx%len(m.articleImages)]
				m.imageViewIdx++
				m.imageViewSrc = img.Src
				m.imageViewAlt = img.Alt
				m.imageRendered = ""
				m.state = stateImageView
				return m, tea.Batch(m.spinner.Tick, fetchBodyImage(img.Src, img.Alt, m.width, m.height))
			}

		case "esc":
			if m.state == stateImageView {
				m.state = stateArticle
				return m, nil
			}
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
						return m, tea.Batch(m.spinner.Tick, fetchArticle(item.link))
					}
				case tabPodcasts:
					if item, ok := m.podcastList.SelectedItem().(podcastEpisode); ok {
						if err := m.player.play(item.audioURL, item.title, item.series, float64(item.duration), 0); err != nil {
							m.err = err
							m.state = stateError
							return m, nil
						}
						m.resize()
						return m, tea.Batch(m.player.waitDone(), pollPlayerPos())
					}
				}
			}

		case " ":
			if m.player.isActive() {
				m.player.togglePause()
				return m, nil
			}

		case "]":
			if m.player.isActive() {
				m.player.seek(30)
				m.player.setPos(min(m.player.getPos()+30, m.player.duration))
				return m, nil
			}

		case "[":
			if m.player.isActive() {
				m.player.seek(-10)
				m.player.setPos(max(m.player.getPos()-10, 0))
				return m, nil
			}

		case "x":
			m.player.stop()
			m.resize()
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
		m.currentHTML = msg.rawHTML
		m.currentArticleImage = msg.imageURL
		m.articleImages = msg.images
		m.inlineImgs = msg.inlineImgs
		m.inlineImgCache = make(map[int]string)
		m.imageViewIdx = 0
		m.rebuildArticleContent()
		m.viewport.GotoTop()
		m.state = stateArticle
		// Start loading inline images in parallel (quiz/visual articles)
		if len(msg.inlineImgs) > 0 {
			cmds := make([]tea.Cmd, len(msg.inlineImgs))
			for i, img := range msg.inlineImgs {
				cmds[i] = fetchInlineImage(i, img.Src, img.Alt, m.width, m.height)
			}
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case inlineImageLoadedMsg:
		if m.inlineImgCache == nil {
			m.inlineImgCache = make(map[int]string)
		}
		m.inlineImgCache[msg.idx] = msg.rendered
		if m.state == stateArticle {
			offset := m.viewport.YOffset
			m.rebuildArticleContent()
			m.viewport.YOffset = offset
		}
		return m, nil

	case playerPosMsg:
		if m.player.isActive() {
			if msg.pos >= 0 {
				m.player.setPos(msg.pos)
			}
			pos := m.player.getPos()
			if pos > 10 && (m.player.duration <= 0 || pos < m.player.duration-30) {
				saveResume(&resumeState{
					AudioURL: m.player.audioURL,
					Title:    m.player.title,
					Series:   m.player.series,
					Duration: m.player.duration,
					Pos:      pos,
				})
			}
			return m, pollPlayerPos()
		}
		return m, nil

	case bodyImageFetchedMsg:
		m.imageRendered = msg.rendered
		return m, nil

	case playerDoneMsg:
		if m.player.isActive() {
			// Natural completion — episode played to end
			m.player.finish()
		}
		// If already stopped (manually killed), don't touch the resume file
		m.resize()
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

// pollPlayerPos waits one second, queries mpv for the current playback position,
// and returns it in a playerPosMsg. Works on all platforms via a simple
// request/response IPC call rather than a persistent event stream.
func pollPlayerPos() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(time.Second)
		return playerPosMsg{pos: queryMpvPos()}
	}
}

// queryMpvPos opens a fresh connection to the mpv IPC socket, sends a single
// get_property request, and returns the time-pos value (or -1 on failure).
func queryMpvPos() float64 {
	conn, err := net.DialTimeout("unix", mpvSock, 300*time.Millisecond)
	if err != nil {
		return -1
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck

	fmt.Fprintf(conn, "{\"command\":[\"get_property\",\"time-pos\"],\"request_id\":1}\n")

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var r struct {
			RequestID int      `json:"request_id"`
			Data      *float64 `json:"data"`
		}
		if json.Unmarshal([]byte(sc.Text()), &r) == nil && r.RequestID == 1 && r.Data != nil {
			return *r.Data
		}
	}
	return -1
}

func (m *model) resize() {
	h, v := docStyle.GetFrameSize()
	playerH := 0
	if m.player.isActive() {
		playerH = 2 // 1 player line + 1 blank separator
	}
	// tab bar(1) + help(1) + separators/spacing(2) = 4
	listH := m.height - v - 4 - playerH
	if listH < 5 {
		listH = 5
	}
	m.articleList.SetSize(m.width-h, listH)
	m.podcastList.SetSize(m.width-h, listH)

	// title(1) + meta(1) + 2×sep(2) + help(1) + spacing(2) = 7
	vpH := m.height - v - 7 - playerH
	if vpH < 5 {
		vpH = 5
	}
	newVPW := m.width - h
	widthChanged := m.viewport.Width != newVPW
	m.viewport.Width = newVPW
	m.viewport.Height = vpH
	if widthChanged && m.state == stateArticle && m.currentHTML != "" {
		m.rebuildArticleContent()
	}
}

// rebuildArticleContent re-renders stored HTML at current viewport width.
func (m *model) rebuildArticleContent() {
	var parts []string
	if m.currentArticleImage != "" {
		if img := renderImage(m.currentArticleImage, "", m.viewport.Width); img != "" {
			parts = append(parts, img)
		}
	}
	if m.currentHTML != "" {
		parts = append(parts, renderHTML(m.currentHTML, m.viewport.Width))
	}
	// Inline images for quiz/visual articles — show each as it loads
	for i, ref := range m.inlineImgs {
		if rendered, ok := m.inlineImgCache[i]; ok {
			parts = append(parts, rendered)
		} else {
			placeholder := imgFallbackStyle.Render(
				fmt.Sprintf("[Laster bilde %d/%d…]", i+1, len(m.inlineImgs)))
			if ref.Alt != "" {
				placeholder += "\n" + captionStyle.Render(ref.Alt)
			}
			parts = append(parts, placeholder)
		}
	}
	if len(parts) > 0 {
		m.viewport.SetContent(strings.Join(parts, "\n\n"))
	}
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
	case stateBrowse:
		return m.browseView()
	case stateArticle:
		return m.articleView()
	case stateImageView:
		return m.imageView()
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

	footer := helpStyle.Render("tab: bytt visning  ·  enter: åpne/spill  ·  /: søk  ·  q: avslutt")

	parts := []string{m.tabBar(), body, "", footer}
	if m.player.isActive() {
		parts = append(parts, "", m.playerBar())
	}
	return docStyle.Render(strings.Join(parts, "\n"))
}

func (m model) articleView() string {
	inner := m.width - 4
	sep := strings.Repeat("─", max(0, inner))
	lines := []string{
		articleTitleStyle.Render(m.currentArticle.title),
		metaStyle.Render(m.currentArticle.author + "  ·  " + m.currentArticle.published),
		sep,
		m.viewport.View(),
		sep,
		helpStyle.Render(m.articleHelpText()),
	}
	if m.player.isActive() {
		lines = append(lines, "", m.playerBar())
	}
	return docStyle.Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

func (m model) articleHelpText() string {
	s := "↑/↓ j/k: scroll  ·  g/G: topp/bunn  ·  esc: tilbake  ·  q: avslutt"
	if n := len(m.inlineImgs); n > 0 {
		loaded := len(m.inlineImgCache)
		if loaded < n {
			s += fmt.Sprintf("  ·  bilder: %d/%d laster", loaded, n)
		} else {
			s += fmt.Sprintf("  ·  %d bilder", n)
		}
	} else if n := len(m.articleImages); n > 0 {
		s += fmt.Sprintf("  ·  i: bilder (%d)", n)
	}
	return s
}

func (m model) imageView() string {
	total := len(m.articleImages)
	idx := (m.imageViewIdx - 1) % total
	counter := fmt.Sprintf("%d/%d", idx+1, total)
	inner := m.width - 4

	if m.imageRendered == "" {
		return docStyle.Render(
			"\n\n" + lipgloss.NewStyle().Width(inner).Align(lipgloss.Center).
				Render(m.spinner.View()+" Laster bilde "+counter+"…"),
		)
	}

	centeredImg := centerBlock(m.imageRendered, inner)
	title := ""
	if m.imageViewAlt != "" {
		title = lipgloss.NewStyle().Width(inner).Align(lipgloss.Center).
			Render(captionStyle.Render(m.imageViewAlt)) + "\n\n"
	}
	help := helpStyle.Render("esc: tilbake  ·  i: neste bilde (" + counter + ")")
	return docStyle.Render(title + centeredImg + "\n\n" + help)
}

// fetchInlineImage downloads and renders one inline image for quiz/visual articles.
func fetchInlineImage(idx int, src, alt string, termWidth, termHeight int) tea.Cmd {
	return func() tea.Msg {
		imgW := termWidth - 4
		if imgW < 20 {
			imgW = 20
		}
		imgH := termHeight / 2
		if imgH < 10 {
			imgH = 10
		}
		rendered := renderImageBounded(src, alt, imgW, imgH)
		if rendered == "" {
			if alt != "" {
				rendered = imgFallbackStyle.Render("[Bilde: " + alt + "]")
			} else {
				rendered = imgFallbackStyle.Render("[Bilde]")
			}
		}
		return inlineImageLoadedMsg{idx: idx, rendered: rendered}
	}
}

// fetchBodyImage downloads and renders a body image via chafa,
// bounded to 75% of the terminal's width and height.
func fetchBodyImage(src, alt string, termWidth, termHeight int) tea.Cmd {
	return func() tea.Msg {
		imgW := (termWidth - 4) * 3 / 4
		if imgW < 20 {
			imgW = 20
		}
		imgH := termHeight * 3 / 4
		if imgH < 10 {
			imgH = 10
		}
		rendered := renderImageBounded(src, alt, imgW, imgH)
		if rendered == "" {
			rendered = imgFallbackStyle.Render("[Kunne ikke laste bilde]")
		}
		return bodyImageFetchedMsg{rendered: rendered}
	}
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
	tabs := lipgloss.JoinHorizontal(lipgloss.Top, art, pod)

	inner := m.width - 4
	kofi := kofiStyle.Render("♥  Støtt spillhistorie  ·  " + kofiURL + "  ♥")
	gap := inner - lipgloss.Width(tabs) - lipgloss.Width(kofi)
	if gap < 1 {
		return tabs
	}
	return tabs + strings.Repeat(" ", gap) + kofi
}

// playerBar renders a single-line footer player with progress bar.
func (m model) playerBar() string {
	inner := m.width - 4 // width inside docStyle margins

	icon := "▶"
	if m.player.status == playerPaused {
		icon = "⏸"
	}

	pos := m.player.getPos()
	posStr := fmtDuration(pos)
	durStr := fmtDuration(m.player.duration)
	controls := "  [-10  ]+30  spc:⏸  x:■"
	sep := " · "

	// Measure fixed portions (rune widths, ASCII-only except icon which is 1 cell)
	fixedW := 1 + 1 + // icon + space
		utf8.RuneCountInString(m.player.series) + len(sep) + // series + sep
		utf8.RuneCountInString(posStr) + 1 + // pos + space
		1 + utf8.RuneCountInString(durStr) + // space + dur
		utf8.RuneCountInString(controls)

	// Remaining width split: 1/3 bar, 2/3 title
	remaining := inner - fixedW
	barW := remaining / 3
	if barW < 4 {
		barW = 4
	}
	titleW := remaining - barW - 1 // -1 for space between title and pos
	if titleW < 0 {
		titleW = 0
	}

	title := clamp(m.player.title, titleW)
	// Pad title to exact titleW so total width is predictable
	titleRunes := utf8.RuneCountInString(title)
	if titleRunes < titleW {
		title += strings.Repeat(" ", titleW-titleRunes)
	}

	pct := 0.0
	if m.player.duration > 0 {
		pct = pos / m.player.duration
	}
	bar := simpleBar(pct, barW)

	line := icon + " " + m.player.series + sep + title + " " + posStr + " " + bar + " " + durStr + controls
	// Pad any remaining space
	lineRunes := utf8.RuneCountInString(icon+" "+m.player.series+sep+title+" "+posStr+" "+bar+" "+durStr+controls)
	if lineRunes < inner {
		line += strings.Repeat(" ", inner-lineRunes)
	}

	return playerBGStyle.Render(line)
}

// simpleBar renders an ASCII progress bar using plain characters (safe width).
func simpleBar(pct float64, width int) string {
	if width <= 0 {
		return ""
	}
	pct = max(0.0, min(1.0, pct))
	filled := int(pct * float64(width))
	if filled > width {
		filled = width
	}
	return strings.Repeat("=", filled) + strings.Repeat("-", width-filled)
}

// renderProgressBar renders a unicode progress bar at the given percentage.
func renderProgressBar(pct float64, width int) string {
	if width <= 0 {
		return ""
	}
	pct = max(0.0, min(1.0, pct))
	filled := int(pct * float64(width))
	rest := width - filled

	var b strings.Builder
	if filled > 0 {
		b.WriteString(progressFilledStyle.Render(strings.Repeat("━", filled)))
	}
	if rest > 0 {
		b.WriteString(progressCursorStyle.Render("╸"))
		if rest > 1 {
			b.WriteString(progressEmptyStyle.Render(strings.Repeat("─", rest-1)))
		}
	} else {
		// At 100%, replace last char with a filled marker
		b.WriteString(progressFilledStyle.Render("━"))
	}
	return b.String()
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// clamp truncates s to n visible runes.
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

// fmtDuration formats seconds as M:SS or H:MM:SS.
func fmtDuration(secs float64) string {
	if secs < 0 {
		secs = 0
	}
	s := int(secs)
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
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

// ─── main ────────────────────────────────────────────────────────────────────

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Feil: %v\n", err)
		os.Exit(1)
	}
}
