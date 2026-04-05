# spillhistorie-tui

A terminal client for [spillhistorie.no](https://spillhistorie.no) — *Litt retro - litt nytt*.

Browse and read articles about gaming history directly in your terminal.

![demo](https://spillhistorie.no/wp-content/uploads/2024/01/logo.png)

## Features

- Fetches the latest articles via RSS
- Opens and renders article content as readable terminal text
- Fuzzy filtering of the article list
- Fully keyboard-driven

## Keybindings

### Article list
| Key | Action |
|-----|--------|
| `↑` / `↓` or `j` / `k` | Navigate |
| `enter` | Open article |
| `/` | Filter/search |
| `q` | Quit |

### Article view
| Key | Action |
|-----|--------|
| `↑` / `↓` or `j` / `k` | Scroll |
| `g` / `G` | Top / bottom |
| `esc` | Back to list |
| `q` | Quit |

## Installation

Requires [Go](https://go.dev) 1.22+.

```sh
go install github.com/jhgundersen/spillhistorie-tui@latest
```

Or build from source:

```sh
git clone https://github.com/jhgundersen/spillhistorie-tui
cd spillhistorie-tui
go build -o spillhistorie-tui .
./spillhistorie-tui
```

## Dependencies

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI framework
- [Bubbles](https://github.com/charmbracelet/bubbles) — UI components
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — Styling
- [Glamour](https://github.com/charmbracelet/glamour) — Markdown rendering
- [gofeed](https://github.com/mmcdole/gofeed) — RSS parsing
- [go-readability](https://github.com/go-shiori/go-readability) — Article extraction
- [html-to-markdown](https://github.com/JohannesKaufmann/html-to-markdown) — HTML conversion
