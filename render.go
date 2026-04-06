package main

// Custom HTML-to-terminal renderer.
// Replaces glamour/html-to-markdown with direct conversion that supports:
// - Proper heading styles (h1–h6)
// - Inline images via chafa (graceful fallback to alt text)
// - Bold, italic, links, code, blockquotes, lists, hr

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wrap"
	"golang.org/x/net/html"
)

// ─── render styles ────────────────────────────────────────────────────────────

var (
	h1Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#C0A0FF"))

	h2Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#9B88FF")).
		Border(lipgloss.NormalBorder(), false, false, true, false).
		BorderForeground(lipgloss.Color("#7D56F4"))

	h3Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#B0A0FF"))

	h4Style = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#AAAACC"))

	rBoldStyle = lipgloss.NewStyle().Bold(true)

	rItalicStyle = lipgloss.NewStyle().Italic(true)

	rLinkStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7D56F4")).
			Underline(true)

	inlineCodeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E8E8E8")).
			Background(lipgloss.Color("#333333")).
			Padding(0, 1)

	codeBlockStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E8E8E8")).
			Background(lipgloss.Color("#1A1A2E")).
			Padding(1, 2)

	blockquoteStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#BBBBBB")).
			BorderLeft(true).
			BorderStyle(lipgloss.ThickBorder()).
			BorderForeground(lipgloss.Color("#7D56F4")).
			PaddingLeft(1)

	captionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888")).
			Italic(true)

	imgFallbackStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#888888")).
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

// renderHTML converts an HTML fragment (from go-readability) to terminal text.
func renderHTML(content string, width int) string {
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
	renderBlockChildren(body, &buf, width)

	result := strings.TrimSpace(buf.String())
	// Collapse runs of 3+ newlines to 2
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return result
}

// ─── block rendering ─────────────────────────────────────────────────────────

func renderBlockChildren(n *html.Node, buf *strings.Builder, width int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderBlock(c, buf, width)
	}
}

func renderBlock(n *html.Node, buf *strings.Builder, width int) {
	if n.Type == html.TextNode {
		if t := strings.TrimSpace(n.Data); t != "" {
			buf.WriteString(t + "\n")
		}
		return
	}
	if n.Type != html.ElementNode {
		renderBlockChildren(n, buf, width)
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
		renderBlockChildren(n, &inner, width-4)
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
		src := bestImgSrc(n)
		alt := elAttr(n, "alt")
		if rendered := renderImage(src, alt, width); rendered != "" {
			buf.WriteString("\n" + rendered + "\n")
		}

	case "figure":
		renderBlockChildren(n, buf, width)

	case "figcaption":
		if t := nodeText(n); t != "" {
			buf.WriteString(captionStyle.Render("  "+t) + "\n\n")
		}

	case "hr":
		line := lipgloss.NewStyle().
			Foreground(lipgloss.Color("#444444")).
			Render(strings.Repeat("─", max(0, width-2)))
		buf.WriteString("\n" + line + "\n\n")

	case "br":
		buf.WriteString("\n")

	default:
		// div, section, article, main, etc.
		renderBlockChildren(n, buf, width)
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
	return wrap.String(t, width)
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
	return ""
}

// normalizeImgURL fixes protocol-relative URLs.
func normalizeImgURL(src string) string {
	if strings.HasPrefix(src, "//") {
		return "https:" + src
	}
	return src
}

func renderImage(src, alt string, width int) string {
	src = normalizeImgURL(src)
	imgLog("renderImage src=%q chafaPath=%q", src, chafaPath)
	if src != "" && chafaPath != "" {
		out, err := chafaRender(src, alt, width)
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

func chafaRender(src, alt string, width int) (string, error) {
	req, err := http.NewRequest("GET", src, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; spillhistorie-tui/1.0)")
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

	var chafaStderr strings.Builder
	cmd := exec.Command(chafaPath,
		"--size", fmt.Sprintf("%d", imgW),
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
	if alt != "" {
		result += "\n" + captionStyle.Render("  "+alt)
	}
	return result, nil
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
