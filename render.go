package main

// Custom HTML-to-terminal renderer.
// Replaces glamour/html-to-markdown with direct conversion that supports:
// - Proper heading styles (h1–h6)
// - Inline images via chafa (graceful fallback to alt text)
// - Bold, italic, links, code, blockquotes, lists, hr

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"golang.org/x/net/html"
)

const (
	podcastCacheMaxBytes = 1 << 30        // 1 GB
	imageCacheMaxBytes   = 100 << 20      // 100 MB
)

// pruneCache removes the oldest files in dir until the total size is under
// maxBytes. Stale .tmp files (older than 1 hour) are always removed first.
func pruneCache(dir string, maxBytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type fileEntry struct {
		path  string
		size  int64
		mtime int64 // unix timestamp for sorting
	}

	var files []fileEntry
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") {
			if time.Since(info.ModTime()) > time.Hour {
				os.Remove(path)
			}
			continue
		}
		files = append(files, fileEntry{path: path, size: info.Size(), mtime: info.ModTime().Unix()})
		total += info.Size()
	}

	if total <= maxBytes {
		return
	}

	// Oldest first.
	sort.Slice(files, func(i, j int) bool { return files[i].mtime < files[j].mtime })

	for _, f := range files {
		if total <= maxBytes {
			break
		}
		os.Remove(f.path)
		total -= f.size
	}
}

// ─── render styles ────────────────────────────────────────────────────────────

var (
	h1Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("13")) // bright magenta

	h2Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("5")). // magenta
		Border(lipgloss.NormalBorder(), false, false, true, false).
		BorderForeground(lipgloss.Color("5"))

	h3Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("13")) // bright magenta

	h4Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("6")) // cyan

	rBoldStyle = lipgloss.NewStyle().Bold(true)

	rItalicStyle = lipgloss.NewStyle().Italic(true)

	rLinkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("4")). // blue
			Underline(true)

	inlineCodeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("8")).
			Padding(0, 1)

	codeBlockStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("0")). // black
			Padding(1, 2)

	blockquoteStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("7")).
			BorderLeft(true).
			BorderStyle(lipgloss.ThickBorder()).
			BorderForeground(lipgloss.Color("5")).
			PaddingLeft(1)

	captionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")). // bright black / gray
			Italic(true)

	imgFallbackStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8")).
				Italic(true)
)

// chafaPath is set at startup if chafa is found in PATH.
var chafaPath string

// imgDebugLog is opened when SPILLHISTORIE_DEBUG=1 is set.
var imgDebugLog *os.File

func init() {
	if p, err := exec.LookPath("chafa"); err == nil {
		chafaPath = p
	}
	if os.Getenv("SPILLHISTORIE_DEBUG") != "" {
		imgDebugLog, _ = os.OpenFile("/tmp/spillhistorie-img.log",
			os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	}
}

func imgLog(format string, args ...interface{}) {
	if imgDebugLog != nil {
		fmt.Fprintf(imgDebugLog, format+"\n", args...)
	}
}

// ─── entry point ─────────────────────────────────────────────────────────────

// inlineImgMarker is written into the rendered text at each <img> position when
// inline=true. rebuildArticleContent splits on it to splice in the real images.
const inlineImgMarker = "\x00IMG\x00"

// renderHTML converts an HTML fragment (from go-readability) to terminal text.
// When inline is true, <img> elements emit inlineImgMarker so the caller can
// splice pre-rendered images in at the correct positions.
func renderHTML(content string, width int, inline bool) string {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return content
	}
	// html.Parse wraps fragments in <html><head/><body>
	body := findEl(doc, "body")
	if body == nil {
		body = doc
	}

	var buf strings.Builder
	renderBlockChildren(body, &buf, width, inline)

	result := strings.TrimSpace(buf.String())
	// Collapse runs of 3+ newlines to 2
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return result
}

// ─── block rendering ─────────────────────────────────────────────────────────

func renderBlockChildren(n *html.Node, buf *strings.Builder, width int, inline bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderBlock(c, buf, width, inline)
	}
}

func renderBlock(n *html.Node, buf *strings.Builder, width int, inline bool) {
	if n.Type == html.TextNode {
		if t := strings.TrimSpace(n.Data); t != "" {
			buf.WriteString(t + "\n")
		}
		return
	}
	if n.Type != html.ElementNode {
		renderBlockChildren(n, buf, width, inline)
		return
	}

	switch n.Data {
	case "script", "style", "nav", "header", "footer", "noscript", "aside":
		return

	case "h1":
		if t := nodeText(n); t != "" {
			buf.WriteString("\n" + h1Style.Render(t) + "\n\n")
		}

	case "h2":
		if t := nodeText(n); t != "" {
			buf.WriteString("\n" + h2Style.Width(width-2).Render(t) + "\n\n")
		}

	case "h3":
		if t := nodeText(n); t != "" {
			buf.WriteString("\n" + h3Style.Render("▸ "+t) + "\n\n")
		}

	case "h4", "h5", "h6":
		if t := nodeText(n); t != "" {
			buf.WriteString("\n" + h4Style.Render(t) + "\n\n")
		}

	case "p":
		// An image-only <p> (pattern: <p><a?><img></a?></p>) should emit a
		// block-level marker in inline mode, not be passed to renderInline.
		if inline && isImageOnlyParagraph(n) {
			buf.WriteString(inlineImgMarker + "\n")
			return
		}
		if t := strings.TrimSpace(renderInline(n, width)); t != "" {
			buf.WriteString(t + "\n\n")
		}

	case "ul":
		buf.WriteString("\n")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == "li" {
				writeBulletItem(buf, renderInline(c, width-4), "  • ", "    ")
			}
		}
		buf.WriteString("\n")

	case "ol":
		buf.WriteString("\n")
		i := 1
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == "li" {
				prefix := fmt.Sprintf("  %d. ", i)
				writeBulletItem(buf, renderInline(c, width-len(prefix)), prefix,
					strings.Repeat(" ", len(prefix)))
				i++
			}
		}
		buf.WriteString("\n")

	case "blockquote":
		var inner strings.Builder
		renderBlockChildren(n, &inner, width-4, inline)
		if t := strings.TrimSpace(inner.String()); t != "" {
			buf.WriteString("\n" + blockquoteStyle.Width(width-4).Render(t) + "\n\n")
		}

	case "pre":
		var raw strings.Builder
		allText(n, &raw)
		if t := raw.String(); t != "" {
			buf.WriteString("\n" + codeBlockStyle.Width(width-2).Render(t) + "\n\n")
		}

	case "img":
		if inline {
			// Emit a marker; rebuildArticleContent will splice in the real image.
			buf.WriteString(inlineImgMarker + "\n")
		}
		// In gallery mode img is suppressed (viewed via i key).
		return

	case "figcaption":
		return

	case "figure":
		renderBlockChildren(n, buf, width, inline)

	case "hr":
		line := lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Render(strings.Repeat("─", max(0, width-2)))
		buf.WriteString("\n" + line + "\n\n")

	case "br":
		buf.WriteString("\n")

	default:
		// div, section, article, main, etc.
		renderBlockChildren(n, buf, width, inline)
	}
}

func writeBulletItem(buf *strings.Builder, text, prefix, cont string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	buf.WriteString(prefix + lines[0] + "\n")
	for _, l := range lines[1:] {
		if l != "" {
			buf.WriteString(cont + l + "\n")
		}
	}
}

// ─── inline rendering ────────────────────────────────────────────────────────

// renderInline renders all inline children of n as a word-wrapped string.
func renderInline(n *html.Node, width int) string {
	var buf strings.Builder
	inlineChildren(n, &buf)
	// Collapse whitespace (preserve explicit \n from <br>)
	raw := buf.String()
	var out strings.Builder
	prevSpace := false
	for _, r := range raw {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				out.WriteRune(' ')
			}
			prevSpace = true
		} else {
			out.WriteRune(r)
			prevSpace = r == '\n'
		}
	}
	t := strings.TrimSpace(out.String())
	if t == "" || width <= 0 {
		return t
	}
	return wordwrap.String(t, width)
}

func inlineChildren(n *html.Node, buf *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		inlineNode(c, buf)
	}
}

func inlineNode(n *html.Node, buf *strings.Builder) {
	switch n.Type {
	case html.TextNode:
		// Map newlines/tabs to spaces for inline context
		buf.WriteString(strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == '\t' {
				return ' '
			}
			return r
		}, n.Data))

	case html.ElementNode:
		switch n.Data {
		case "script", "style":
			return
		case "strong", "b":
			var inner strings.Builder
			inlineChildren(n, &inner)
			if t := strings.TrimSpace(inner.String()); t != "" {
				buf.WriteString(rBoldStyle.Render(t))
			}
		case "em", "i":
			var inner strings.Builder
			inlineChildren(n, &inner)
			if t := strings.TrimSpace(inner.String()); t != "" {
				buf.WriteString(rItalicStyle.Render(t))
			}
		case "a":
			var inner strings.Builder
			inlineChildren(n, &inner)
			if t := strings.TrimSpace(inner.String()); t != "" {
				buf.WriteString(rLinkStyle.Render(t))
			}
		case "code":
			var inner strings.Builder
			inlineChildren(n, &inner)
			if t := strings.TrimSpace(inner.String()); t != "" {
				buf.WriteString(inlineCodeStyle.Render(t))
			}
		case "br":
			buf.WriteString("\n")
		case "img":
			if alt := elAttr(n, "alt"); alt != "" {
				buf.WriteString(imgFallbackStyle.Render("[" + alt + "]"))
			}
		default:
			inlineChildren(n, buf)
		}
	}
}

// ─── image rendering ─────────────────────────────────────────────────────────

// bestImgSrc returns the most useful src from an <img> node,
// preferring real URLs over data: placeholders.
func bestImgSrc(n *html.Node) string {
	candidates := []string{
		elAttr(n, "src"),
		elAttr(n, "data-src"),
		elAttr(n, "data-lazy-src"),
		elAttr(n, "data-original"),
	}
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		if s != "" && !strings.HasPrefix(s, "data:") {
			return s
		}
	}
	// Fall back to the first URL in srcset
	if ss := elAttr(n, "srcset"); ss != "" {
		for _, part := range strings.Split(ss, ",") {
			if fields := strings.Fields(strings.TrimSpace(part)); len(fields) > 0 {
				if u := fields[0]; u != "" && !strings.HasPrefix(u, "data:") {
					return u
				}
			}
		}
	}
	return ""
}

// centerBlock pads each line of content with spaces on the left to center it
// within totalWidth columns.
func centerBlock(content string, totalWidth int) string {
	if content == "" {
		return content
	}
	lines := strings.Split(content, "\n")
	blockW := 0
	for _, l := range lines {
		if w := lipgloss.Width(l); w > blockW {
			blockW = w
		}
	}
	pad := (totalWidth - blockW) / 2
	if pad <= 0 {
		return content
	}
	padStr := strings.Repeat(" ", pad)
	for i, l := range lines {
		lines[i] = padStr + l
	}
	return strings.Join(lines, "\n")
}

// normalizeImgURL fixes protocol-relative URLs.
func normalizeImgURL(src string) string {
	if strings.HasPrefix(src, "//") {
		return "https:" + src
	}
	return src
}

func renderImage(src, alt string, width int) string {
	return renderImageBounded(src, alt, width, 0)
}

func renderImageBounded(src, alt string, width, maxHeight int) string {
	src = normalizeImgURL(src)
	imgLog("renderImage src=%q chafaPath=%q", src, chafaPath)
	if src != "" && chafaPath != "" {
		out, err := chafaRender(src, alt, width, maxHeight)
		imgLog("chafaRender err=%v outLen=%d", err, len(out))
		if err == nil && out != "" {
			return out
		}
	}
	if alt != "" {
		return imgFallbackStyle.Render("[Bilde: " + alt + "]")
	}
	if src != "" {
		return imgFallbackStyle.Render("[Bilde]")
	}
	return ""
}

// chafaCacheDir returns the directory used for on-disk chafa output cache,
// creating it if necessary. Returns "" on any error.
func chafaCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(base, "spillhistorie", "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// chafaCacheKey returns a filename-safe cache key for a given src/width/height.
func chafaCacheKey(src string, width, maxHeight int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", src, width, maxHeight)))
	return fmt.Sprintf("%x", h)
}

func chafaRender(src, alt string, width, maxHeight int) (string, error) {
	// Check disk cache first (keyed on URL + dimensions, alt is appended after).
	cacheDir := chafaCacheDir()
	cacheKey := chafaCacheKey(src, width, maxHeight)
	if cacheDir != "" {
		if data, err := os.ReadFile(filepath.Join(cacheDir, cacheKey)); err == nil {
			result := string(data)
			if alt != "" {
				result += "\n" + captionStyle.Render("  "+alt)
			}
			return result, nil
		}
	}

	req, err := http.NewRequest("GET", src, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; spillhistorie/1.0)")
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	req.Header.Set("Referer", "https://spillhistorie.no/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ext := ".jpg"
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "png"):
		ext = ".png"
	case strings.Contains(ct, "gif"):
		ext = ".gif"
	case strings.Contains(ct, "webp"):
		ext = ".webp"
	}

	tmp, err := os.CreateTemp("", "spillhistorie-*"+ext)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	imgW := width - 4
	if imgW < 10 {
		imgW = 10
	}
	sizeArg := fmt.Sprintf("%d", imgW)
	if maxHeight > 0 {
		sizeArg = fmt.Sprintf("%dx%d", imgW, maxHeight)
	}

	var chafaStderr strings.Builder
	cmd := exec.Command(chafaPath,
		"--size", sizeArg,
		"--format", "symbol",
		"--symbols", "block+border+space",
		tmp.Name(),
	)
	cmd.Stderr = &chafaStderr
	out, err := cmd.Output()
	if err != nil {
		imgLog("chafa stderr: %s", chafaStderr.String())
		return "", err
	}

	result := strings.TrimRight(string(out), "\n")

	// Persist raw render to disk cache (without caption — alt may vary per call).
	if cacheDir != "" {
		_ = os.WriteFile(filepath.Join(cacheDir, cacheKey), []byte(result), 0o644)
		pruneCache(cacheDir, imageCacheMaxBytes)
	}

	if alt != "" {
		result += "\n" + captionStyle.Render("  "+alt)
	}
	return result, nil
}

// ─── podcast cache ────────────────────────────────────────────────────────────

// podcastCachePath returns the path where an episode should be cached on disk.
// Returns "" if the cache directory cannot be determined.
func podcastCachePath(audioURL string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(base, "spillhistorie", "podcasts")
	h := sha256.Sum256([]byte(audioURL))
	return filepath.Join(dir, fmt.Sprintf("%x", h))
}

// cacheEpisodeAsync downloads audioURL to the podcast cache in a background
// goroutine. It writes to a .tmp file first and renames atomically on success,
// so a partially-downloaded file is never mistaken for a complete one.
func cacheEpisodeAsync(audioURL string) {
	go func() {
		path := podcastCachePath(audioURL)
		if path == "" {
			return
		}
		if _, err := os.Stat(path); err == nil {
			return // already cached
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		tmp := path + ".tmp"
		resp, err := http.Get(audioURL)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		f, err := os.Create(tmp)
		if err != nil {
			return
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			f.Close()
			os.Remove(tmp)
			return
		}
		f.Close()
		os.Rename(tmp, path)
		pruneCache(filepath.Dir(path), podcastCacheMaxBytes)
	}()
}

// ─── HTML helpers ─────────────────────────────────────────────────────────────

// nodeText returns the plain text content of a node (strips tags).
func nodeText(n *html.Node) string {
	var buf strings.Builder
	allText(n, &buf)
	return strings.TrimSpace(buf.String())
}

func allText(n *html.Node, buf *strings.Builder) {
	if n.Type == html.TextNode {
		buf.WriteString(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		allText(c, buf)
	}
}

func elAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// ImageRef holds a body image's URL and alt text for lazy loading.
type ImageRef struct {
	Src string
	Alt string
}

// ExtractArticleImages walks parsed HTML and returns all body images in order.
func ExtractArticleImages(content string) []ImageRef {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return nil
	}
	var images []ImageRef
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			if src := bestImgSrc(n); src != "" {
				images = append(images, ImageRef{Src: src, Alt: elAttr(n, "alt")})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return images
}

// isImageOnlyParagraph returns true when a <p> contains an <img> but no
// visible text — the WordPress pattern for inline quiz/gallery images.
func isImageOnlyParagraph(n *html.Node) bool {
	return strings.TrimSpace(nodeText(n)) == "" && hasDescendantImg(n)
}

func hasDescendantImg(n *html.Node) bool {
	if n.Type == html.ElementNode && n.Data == "img" {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasDescendantImg(c) {
			return true
		}
	}
	return false
}

// ExtractInlineImages returns images in the same order and count as the
// inlineImgMarker tokens emitted by renderHTML(..., inline=true).
// It must stay in sync with the renderBlock logic.
func ExtractInlineImages(content string) []ImageRef {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return nil
	}
	body := findEl(doc, "body")
	if body == nil {
		body = doc
	}

	var images []ImageRef
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}
		switch n.Data {
		case "script", "style", "nav", "header", "footer", "noscript", "aside":
			return
		case "p":
			if isImageOnlyParagraph(n) {
				// Collect the single img inside this paragraph
				var findImg func(*html.Node)
				findImg = func(m *html.Node) {
					if m.Type == html.ElementNode && m.Data == "img" {
						if src := bestImgSrc(m); src != "" {
							images = append(images, ImageRef{Src: src, Alt: elAttr(m, "alt")})
						}
						return
					}
					for c := m.FirstChild; c != nil; c = c.NextSibling {
						findImg(c)
					}
				}
				findImg(n)
				return // don't recurse further into this paragraph
			}
			// paragraph with real text — skip any imgs inside it
			return
		case "img":
			// block-level img (e.g. directly in <figure> or <div>)
			if src := bestImgSrc(n); src != "" {
				images = append(images, ImageRef{Src: src, Alt: elAttr(n, "alt")})
			}
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(body)
	return images
}

func findEl(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findEl(c, tag); found != nil {
			return found
		}
	}
	return nil
}

// findElByClass finds the first element with the given tag and CSS class.
func findElByClass(n *html.Node, tag, class string) *html.Node {
	if n.Type == html.ElementNode && (tag == "" || n.Data == tag) {
		for _, a := range n.Attr {
			if a.Key == "class" {
				for _, c := range strings.Fields(a.Val) {
					if c == class {
						return n
					}
				}
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElByClass(c, tag, class); found != nil {
			return found
		}
	}
	return nil
}

// findContentRoot locates the main article content node in a full HTML document.
// Tries <article>, then common WordPress content div classes.
func findContentRoot(doc *html.Node) *html.Node {
	if n := findEl(doc, "article"); n != nil {
		return n
	}
	if n := findElByClass(doc, "div", "entry-content"); n != nil {
		return n
	}
	if n := findElByClass(doc, "div", "post-content"); n != nil {
		return n
	}
	return nil
}

// ExtractPageBodyImages extracts images from only the main article content area
// of a raw HTML page, skipping navigation, headers, footers, and sidebars.
func ExtractPageBodyImages(rawHTML string) []ImageRef {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil
	}
	root := findContentRoot(doc)
	if root == nil {
		// Fall back to body but skip structural noise
		root = findEl(doc, "body")
		if root == nil {
			root = doc
		}
	}

	var images []ImageRef
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "nav", "header", "footer", "aside", "script", "style":
				return // skip these subtrees
			case "img":
				if src := bestImgSrc(n); src != "" {
					images = append(images, ImageRef{Src: src, Alt: elAttr(n, "alt")})
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return images
}
