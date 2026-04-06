# spillhistorie-tui

A terminal client for [spillhistorie.no](https://spillhistorie.no) — *Litt retro - litt nytt*.

Browse articles and listen to podcasts about gaming history directly in your terminal.

## Features

- Browse and read articles via RSS
- Listen to podcast episodes (Diskettkameratene, cd SPILL)
- Article images rendered in the terminal (requires chafa)
- Fuzzy filtering of article and podcast lists
- Resume podcast playback from where you left off
- Fully keyboard-driven, adapts to terminal color scheme

## Requirements

### Required

- [Go](https://go.dev) 1.22+ (to build)

### Optional

| Tool | Feature |
|------|---------|
| [mpv](https://mpv.io) | Podcast playback |
| [chafa](https://hpjansson.org/chafa/) | Article images rendered in the terminal |

#### Installing mpv

| Platform | Command |
|----------|---------|
| Arch Linux | `sudo pacman -S mpv` |
| Debian / Ubuntu | `sudo apt install mpv` |
| Fedora | `sudo dnf install mpv` |
| Homebrew (macOS) | `brew install mpv` |
| Windows (Scoop) | `scoop install mpv` |

#### Installing chafa

| Platform | Command |
|----------|---------|
| Arch Linux | `sudo pacman -S chafa` |
| Debian / Ubuntu | `sudo apt install chafa` |
| Fedora | `sudo dnf install chafa` |
| Homebrew (macOS) | `brew install chafa` |

## Installation

### One-liner (Linux / macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/jhgundersen/spillhistorie-tui/main/install.sh | sh
```

Downloads the latest release binary for your OS and architecture to `/usr/local/bin` (or `~/.local/bin` if you don't have write access there).

### Via Go

```sh
go install github.com/jhgundersen/spillhistorie-tui@latest
```

### Build from source

```sh
git clone https://github.com/jhgundersen/spillhistorie-tui
cd spillhistorie-tui
go build -o spillhistorie-tui .
./spillhistorie-tui
```

## Keybindings

### Browse (articles / podcasts)

| Key | Action |
|-----|--------|
| `tab` | Switch between Articles and Podcasts |
| `↑` / `↓` or `j` / `k` | Navigate list |
| `enter` | Open article / play episode |
| `/` | Filter / search |
| `q` | Quit |

### Article view

| Key | Action |
|-----|--------|
| `↑` / `↓` or `j` / `k` | Scroll |
| `g` / `G` | Top / bottom |
| `i` | Show next image in popup (when images are available) |
| `esc` | Back to list |
| `q` | Quit |

### Podcast player

| Key | Action |
|-----|--------|
| `space` | Pause / resume |
| `[` | Seek back 10 s |
| `]` | Seek forward 30 s |
| `x` | Stop (saves position for resuming) |
| `q` | Quit (saves position for resuming) |

## Go dependencies

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) — TUI framework
- [Bubbles](https://github.com/charmbracelet/bubbles) — UI components (list, viewport, spinner)
- [Lip Gloss](https://github.com/charmbracelet/lipgloss) — Styling
- [gofeed](https://github.com/mmcdole/gofeed) — RSS / podcast feed parsing
- [go-readability](https://github.com/go-shiori/go-readability) — Article content extraction
