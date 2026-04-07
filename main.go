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
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─── constants ────────────────────────────────────────────────────────────────

// version is set at build time via -ldflags "-X main.version=v1.2.3".
var version = "dev"

const (
	kofiURL = "https://ko-fi.com/joachimfroholt"
	mpvSock = "/tmp/spillhistorie.sock"
)

type articleCategory struct {
	name       string
	categoryID int   // WP category ID, 0 = no filter
	tagIDs     []int // WP tag IDs (OR filter), nil = no filter
}

var articleCategories = []articleCategory{
	{name: "Framside"},
	{name: "Nye spill", categoryID: 3},
	{name: "Retro", categoryID: 4},
	{name: "Indie", categoryID: 1044},
	{name: "Inntrykk", categoryID: 1038},
	{name: "Features", categoryID: 2892},
	{name: "Quiz", tagIDs: []int{300, 6750}},
}

var podcastFeeds = []struct{ name, url string }{
	{"Diskettkameratene", "https://feeds.transistor.fm/diskettkameratene"},
	{"cd SPILL", "https://feed.podbean.com/cdspill/feed.xml"},
	{"Spæll", "https://feed.podbean.com/spaell/feed.xml"},
	{"Pappskaller", "https://anchor.fm/s/10b427ba4/podcast/rss"},
}

var podcastCategories = []string{
	"Alle",
	"Diskettkameratene",
	"cd SPILL",
	"Spæll",
	"Pappskaller",
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

	kofiStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	docStyle = lipgloss.NewStyle().Margin(1, 2)
)

// ─── app state ────────────────────────────────────────────────────────────────

type appState int

const (
	stateArticlesLoading appState = iota
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
	id            int
	title         string
	link          string
	author        string
	published     string
	tagIDs        []int  // WP tag IDs (for quiz detection)
	content       string // pre-loaded HTML from Phase 2
	featuredImage string // pre-loaded from Phase 2
}

func (a article) Title() string { return a.title }
func (a article) Description() string {
	if a.author != "" {
		return a.author + "  ·  " + a.published
	}
	return a.published
}
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
	cmd        *exec.Cmd
	status     playerStatus
	audioURL   string
	title      string
	series     string
	done       chan struct{}
	pos        float64 // protected by posSync; use getPos/setPos
	posSync    sync.Mutex
	duration   float64 // seconds, from RSS
	buffering  bool    // true until the first valid position is received
	generation int     // incremented on each play(); used to discard stale messages
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

	// Play from local cache if available; otherwise stream and cache in background.
	playURL := audioURL
	if path := podcastCachePath(audioURL); path != "" {
		if _, err := os.Stat(path); err == nil {
			playURL = path
		} else {
			cacheEpisodeAsync(audioURL)
		}
	}

	args := []string{
		"--no-video", "--no-terminal", "--quiet",
		"--input-ipc-server=" + mpvSock,
	}
	if startPos > 0 {
		args = append(args, fmt.Sprintf("--start=%g", startPos))
	}
	args = append(args, playURL)
	cmd := exec.Command("mpv", args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	p.generation++
	p.cmd = cmd
	p.audioURL = audioURL
	p.title = title
	p.series = series
	p.duration = duration
	p.setPos(startPos)
	p.status = playerPlaying
	p.buffering = true
	p.done = done
	go func() { cmd.Wait(); close(done) }()
	return nil
}

func (p *player) waitDone() tea.Cmd {
	done := p.done
	gen := p.generation
	if done == nil {
		return nil
	}
	return func() tea.Msg { <-done; return playerDoneMsg{generation: gen} }
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

// cancel stops playback and removes any resume state (explicit user stop via 'x').
// Unlike stop(), it does not save the current position for later resuming.
func (p *player) cancel() {
	deleteResume()
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
	p.buffering = false
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

type articlesFetchedMsg []article

type articleEnrichment struct {
	author        string
	content       string
	featuredImage string
}

type articlesContentMsg struct {
	categoryIdx int
	byID        map[int]articleEnrichment
}
type podcastsFetchedMsg []podcastEpisode
type articleFetchedMsg struct {
	title      string
	rawHTML    string
	imageURL   string
	images     []ImageRef // gallery images (regular articles)
	inlineImgs []ImageRef // inline images (quiz/visual articles)
}
type playerDoneMsg struct{ generation int }
type playerPosMsg struct {
	pos        float64 // pos < 0 means query failed
	generation int
}
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
	articleCategoryIdx  int
	articlesLoading     bool
	articleListCache    map[int][]article
	podcastCategoryIdx  int
	allEpisodes         []podcastEpisode
	articleList         list.Model
	podcastList         list.Model
	viewport            viewport.Model
	spinner             spinner.Model
	player              *player
	playerProgress      progress.Model
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
	al.SetShowTitle(false)
	al.SetShowStatusBar(true)
	al.SetFilteringEnabled(true)

	pl := list.New([]list.Item{}, del, 0, 0)
	pl.SetShowTitle(false)
	pl.SetShowStatusBar(true)
	pl.SetFilteringEnabled(true)

	prog := progress.New(
		progress.WithGradient("#af00af", "#ff87ff"),
		progress.WithoutPercentage(),
	)

	return model{
		state:            stateArticlesLoading,
		articlesLoading:  true,
		articleListCache: make(map[int][]article),
		spinner:          s,
		articleList:      al,
		podcastList:      pl,
		player:           &player{},
		playerProgress:   prog,
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
	cmds := []tea.Cmd{m.spinner.Tick, fetchArticleList(articleCategories[0])}
	if rs := loadResume(); rs != nil {
		if err := m.player.play(rs.AudioURL, rs.Title, rs.Series, rs.Duration, rs.Pos); err == nil {
			cmds = append(cmds, m.player.waitDone(), pollPlayerPos(m.player.generation))
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

		case "h", "left":
			if m.state == stateBrowse && !m.articlesLoading {
				if m.tab == tabArticles && m.articleCategoryIdx > 0 {
					if cmd := m.switchArticleCategory(m.articleCategoryIdx - 1); cmd != nil {
						return m, cmd
					}
				} else if m.tab == tabPodcasts && m.podcastsLoaded && m.podcastCategoryIdx > 0 {
					m.podcastCategoryIdx--
					m.podcastList.SetItems(m.filteredPodcastItems())
				}
			}

		case "l", "right":
			if m.state == stateBrowse && !m.articlesLoading {
				if m.tab == tabArticles && m.articleCategoryIdx < len(articleCategories)-1 {
					if cmd := m.switchArticleCategory(m.articleCategoryIdx + 1); cmd != nil {
						return m, cmd
					}
				} else if m.tab == tabPodcasts && m.podcastsLoaded && m.podcastCategoryIdx < len(podcastCategories)-1 {
					m.podcastCategoryIdx++
					m.podcastList.SetItems(m.filteredPodcastItems())
				}
			}

		case "r":
			if m.state == stateBrowse && m.tab == tabArticles && !m.articlesLoading {
				delete(m.articleListCache, m.articleCategoryIdx)
				m.articlesLoading = true
				return m, tea.Batch(m.spinner.Tick, fetchArticleList(articleCategories[m.articleCategoryIdx]))
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
						return m, tea.Batch(m.spinner.Tick, fetchArticle(item))
					}
				case tabPodcasts:
					if item, ok := m.podcastList.SelectedItem().(podcastEpisode); ok {
						if err := m.player.play(item.audioURL, item.title, item.series, float64(item.duration), 0); err != nil {
							m.err = err
							m.state = stateError
							return m, nil
						}
						m.resize()
						return m, tea.Batch(m.player.waitDone(), pollPlayerPos(m.player.generation))
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
			m.player.cancel()
			m.resize()
			return m, nil
		}

	case articlesFetchedMsg:
		articles := []article(msg)
		m.articleListCache[m.articleCategoryIdx] = articles
		items := make([]list.Item, len(articles))
		for i, a := range articles {
			items[i] = a
		}
		m.articleList.SetItems(items)
		m.articlesLoading = false
		m.state = stateBrowse
		// Phase 2: background-fetch content + author + image
		ids := make([]int, len(articles))
		for i, a := range articles {
			ids[i] = a.id
		}
		return m, fetchArticleContent(m.articleCategoryIdx, ids)

	case articlesContentMsg:
		cached, ok := m.articleListCache[msg.categoryIdx]
		if !ok {
			return m, nil
		}
		for i, a := range cached {
			if e, ok := msg.byID[a.id]; ok {
				cached[i].author = e.author
				cached[i].content = e.content
				cached[i].featuredImage = e.featuredImage
			}
		}
		m.articleListCache[msg.categoryIdx] = cached
		if m.articleCategoryIdx == msg.categoryIdx {
			items := make([]list.Item, len(cached))
			for i, a := range cached {
				items[i] = a
			}
			m.articleList.SetItems(items)
		}
		return m, nil

	case podcastsFetchedMsg:
		m.allEpisodes = []podcastEpisode(msg)
		m.podcastsLoaded = true
		m.podcastsLoading = false
		m.podcastList.SetItems(m.filteredPodcastItems())
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
		if m.player.isActive() && m.player.generation == msg.generation {
			if msg.pos >= 0 {
				m.player.setPos(msg.pos)
				m.player.buffering = false
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
			return m, pollPlayerPos(m.player.generation)
		}
		return m, nil

	case bodyImageFetchedMsg:
		m.imageRendered = msg.rendered
		return m, nil

	case playerDoneMsg:
		if m.player.isActive() && m.player.generation == msg.generation {
			// Natural completion — episode played to end
			m.player.finish()
		}
		// Stale message (old episode) or already stopped — ignore
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
func pollPlayerPos(gen int) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(time.Second)
		return playerPosMsg{pos: queryMpvPos(), generation: gen}
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
	// tab bar(1) + category bar(1 when applicable) + help(1) + separators/spacing(2) = 4 or 5
	catBar := 0
	if m.tab == tabArticles || (m.tab == tabPodcasts && m.podcastsLoaded) {
		catBar = 1
	}
	listH := m.height - v - 4 - playerH - catBar
	if listH < 5 {
		listH = 5
	}
	m.articleList.SetSize(m.width-h, listH)
	m.podcastList.SetSize(m.width-h, listH)

	// headerBar(1) + title(1) + meta(1) + 2×sep(2) + help(1) + spacing(2) = 8
	vpH := m.height - v - 8 - playerH
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
// For inline-image articles (quizzes etc.) it splices rendered images into the
// positions where <img> tags appeared in the original HTML.
func (m *model) rebuildArticleContent() {
	var sb strings.Builder

	if m.currentArticleImage != "" {
		if img := renderImage(m.currentArticleImage, "", m.viewport.Width); img != "" {
			sb.WriteString(img)
			sb.WriteString("\n\n")
		}
	}

	if m.currentHTML != "" {
		inline := len(m.inlineImgs) > 0
		text := renderHTML(m.currentHTML, m.viewport.Width, inline)

		if inline {
			// Split on the marker emitted for each <img> and splice in images.
			segments := strings.Split(text, inlineImgMarker)
			for i, seg := range segments {
				if t := strings.TrimSpace(seg); t != "" {
					sb.WriteString(t)
					sb.WriteString("\n\n")
				}
				if i < len(m.inlineImgs) {
					ref := m.inlineImgs[i]
					if rendered, ok := m.inlineImgCache[i]; ok {
						sb.WriteString(rendered)
					} else {
						placeholder := imgFallbackStyle.Render(
							fmt.Sprintf("[Laster bilde %d/%d…]", i+1, len(m.inlineImgs)))
						if ref.Alt != "" {
							placeholder += "\n" + captionStyle.Render(ref.Alt)
						}
						sb.WriteString(placeholder)
					}
					sb.WriteString("\n\n")
				}
			}
		} else {
			sb.WriteString(text)
		}
	}

	if sb.Len() > 0 {
		m.viewport.SetContent(strings.TrimSpace(sb.String()))
	}
}

// ─── view ─────────────────────────────────────────────────────────────────────

func (m model) View() string {
	if m.width == 0 {
		return ""
	}
	switch m.state {
	case stateArticlesLoading:
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
		if m.articlesLoading {
			body = "\n  " + m.spinner.View() + " Laster artikler…\n"
		} else {
			body = m.articleList.View()
		}
	} else if m.podcastsLoading {
		body = "\n  " + m.spinner.View() + " Laster podcaster…\n"
	} else {
		body = m.podcastList.View()
	}

	footer := helpStyle.Render("tab: bytt visning  ·  h/l: kategori  ·  r: oppdater  ·  enter: åpne/spill  ·  /: søk  ·  q: avslutt")

	parts := []string{m.tabBar()}
	if m.tab == tabArticles {
		parts = append(parts, m.categoryBar())
	} else if m.tab == tabPodcasts && m.podcastsLoaded {
		parts = append(parts, m.podcastCategoryBar())
	}
	parts = append(parts, body, "", footer)
	if m.player.isActive() {
		parts = append(parts, "", m.playerBar())
	}
	return docStyle.Render(strings.Join(parts, "\n"))
}

func (m model) categoryBar() string {
	var tabs []string
	for i, cat := range articleCategories {
		if i == m.articleCategoryIdx {
			tabs = append(tabs, activeTabStyle.Render(cat.name))
		} else {
			tabs = append(tabs, inactiveTabStyle.Render(cat.name))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
}

func (m model) podcastCategoryBar() string {
	var tabs []string
	for i, name := range podcastCategories {
		if i == m.podcastCategoryIdx {
			tabs = append(tabs, activeTabStyle.Render(name))
		} else {
			tabs = append(tabs, inactiveTabStyle.Render(name))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
}

// switchArticleCategory updates articleCategoryIdx and either loads from cache
// (instant) or triggers a network fetch. Returns the cmd to run (nil if cached).
func (m *model) switchArticleCategory(idx int) tea.Cmd {
	m.articleCategoryIdx = idx
	if cached, ok := m.articleListCache[idx]; ok {
		items := make([]list.Item, len(cached))
		for i, a := range cached {
			items[i] = a
		}
		m.articleList.SetItems(items)
		return nil
	}
	m.articlesLoading = true
	return tea.Batch(m.spinner.Tick, fetchArticleList(articleCategories[idx]))
}

func (m model) filteredPodcastItems() []list.Item {
	cat := podcastCategories[m.podcastCategoryIdx]
	var items []list.Item
	for _, e := range m.allEpisodes {
		if cat == "Alle" || e.series == cat {
			items = append(items, e)
		}
	}
	return items
}

func (m model) articleHeaderBar() string {
	back := inactiveTabStyle.Render("← Artikler")
	return rightAlign(back, m.kofiMsg(), m.width-4)
}

func (m model) articleView() string {
	inner := m.width - 4
	sep := strings.Repeat("─", max(0, inner))
	lines := []string{
		m.articleHeaderBar(),
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

// rightAlign places left flush-left and right flush-right within width,
// padding with spaces. If there is not enough room for both, left wins.
func rightAlign(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m model) kofiMsg() string {
	return kofiStyle.Render("♥  Støtt spillhistorie  ·  " + kofiURL + "  ♥")
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
	return rightAlign(tabs, m.kofiMsg(), m.width-4)
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
	titleRunes := utf8.RuneCountInString(title)
	if titleRunes < titleW {
		title += strings.Repeat(" ", titleW-titleRunes)
	}

	pct := 0.0
	if m.player.duration > 0 {
		pct = pos / m.player.duration
	}

	var bar string
	var barVisualW int
	if m.player.buffering {
		label := m.spinner.View() + " Laster…"
		bar = label
		barVisualW = barW // pad to same slot
		labelW := lipgloss.Width(label)
		if labelW < barW {
			bar += strings.Repeat(" ", barW-labelW)
		}
	} else {
		// Use a local copy so we can set width without mutating m.
		prog := m.playerProgress
		prog.Width = barW
		bar = prog.ViewAs(pct)
		barVisualW = barW
	}

	line := icon + " " + m.player.series + sep + title + " " + posStr + " " + bar + " " + durStr + controls
	// Use known widths (not RuneCountInString) since bar may contain ANSI codes.
	knownW := fixedW + titleW + 1 + barVisualW + 1 // +1 title-pos space, +1 bar-dur space
	if knownW < inner {
		line += strings.Repeat(" ", inner-knownW)
	}

	return playerBGStyle.Render(line)
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
