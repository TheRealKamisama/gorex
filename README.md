# GoRex

A replica of [Superlogical's Rex](https://www.superlogical.com/updates/public-testing-beginning)
terminal, written in Go with [MyGo](https://mygo.egoist.dev)'s native UI and its
terminal plugin (Ghostty's libghostty-vt). No webview, no cgo: one ~15 MB app.

![GoRex](docs/screenshot.png)

## What it does

- **Tabs and split panes.** Every pane is a card over the window's gradient,
  with its program's icon, name and directory in its header, and buttons to
  split right, split down, zoom and close. Drag the gaps between panes to
  resize them (double-click resets a split to half).
- **Persistent sessions.** Shells run in a session server, a background
  process of the same binary (`GoRex -server`). Quit GoRex and everything
  keeps running; open it again and every tab, split and pane comes back as
  it was, programs still running. The server keeps each session's screen in
  a headless libghostty-vt emulator and sends a snapshot on attach, so
  full-screen programs (lazygit, vim, htop…) come back exactly.
- **Program activity.** The server watches each terminal's foreground
  process (`tcgetpgrp`, `sysctl`, `proc_pidinfo`; on Windows, the shell's
  newest child, its command line and directory): headers and tabs name the
  program (`Node`, `Git Changes` for lazygit, `Codex`, `SSH host`…) and its
  working directory, a green dot shows a program printing, and an orange dot
  marks a pane whose program finished or rang the bell out of sight. When
  the window is in the background, a long command finishing posts a
  notification.
- **Tab icons** stack the tiles of a tab's programs, the focused pane's in
  front, as Rex does. Tabs reorder by dragging, rename by double-click, and
  have a context menu.
- **Remote hosts.** The host chip switches the window between this
  machine and others reached through ssh, each with tabs of its own whose
  sessions live in that machine's GoRex server: lose the link, close the
  laptop, and they go on there, and come back as they were when GoRex
  connects again (see [Remote hosts](#remote-hosts)).
- **Host chip** with the machine's name and model; its popover shows the
  chip, memory, OS, the session server, the link's latency, and the other
  hosts.
- **Command palette** (⇧⌘P, Ctrl+Shift+P, or the ⌘ button): every command, and every pane
  to jump to, fuzzy-matched.
- Light and dark appearances (View ▸ Appearance), text size (⌘+ ⌘− ⌘0),
  JetBrains Mono embedded.

## Shortcuts

| macOS | Windows, Linux | |
|---|---|---|
| ⌘T | Ctrl+Shift+T | New tab |
| ⌘D / ⇧⌘D | Ctrl+Shift+D / Ctrl+Shift+E | Split right / down |
| ⌘W / ⇧⌘W | Ctrl+Shift+W | Close pane / tab |
| ⇧⌘↩ | Ctrl+Shift+Enter | Zoom the pane |
| ⌥⌘ arrows | Alt+arrows | Focus the pane in that direction |
| ⌃⌘ arrows | Alt+Shift+arrows | Move the nearest divider |
| ⌃⌘= | | Equalize panes |
| ⌘1…⌘9, ⇧⌘[ ⇧⌘], ⌃Tab | Ctrl+Shift+1…9, Ctrl+PageUp/PageDown, Ctrl+Tab | Switch tabs |
| ⇧⌘R | Ctrl+Shift+R | Rename tab |
| ⇧⌘P, ⌘P | Ctrl+Shift+P | Command palette |
| ⌘K | Ctrl+Shift+K | Clear |
| ⌘+ ⌘− ⌘0 | Ctrl+= Ctrl+- Ctrl+0 | Text size |
| ⌥⌘Q | | Quit and end all sessions |

Elsewhere than on macOS, Control and a letter are the terminal's (Ctrl+C,
Ctrl+D, Ctrl+W…), so GoRex's shortcuts take Shift too, as copying and
pasting do (Ctrl+Shift+C, Ctrl+Shift+V), and none take Control and Alt,
which type characters (AltGr) on many keyboards.

## Develop and build

GoRex needs [Go](https://go.dev/dl/) 1.27+ and MyGo 0.3.0, whose CLI
`go.mod` pins as a tool:

```sh
go tool mygo dev     # GoRex Dev, rebuilt and restarted as the code changes
go test ./...        # the server, and the view without a window
go tool mygo build   # build/darwin-arm64/GoRex.app and a .dmg
go run ./tools/mkicon  # redraw resources/icon.png
```

`go get -tool github.com/egoist/mygo/cmd/mygo@latest` updates MyGo and its
CLI together.

The app keeps its state in its data directory: the server's socket and
log, `layout.json` and `settings.json`, in `~/Library/Application
Support/GoRex` for the built app and `GoRex Dev` for `mygo dev`'s, so
that developing never touches the sessions of the app you use
(`GOREX_DIR` names another directory). The sessions outlive the app, but
not a rebuild: a development build replaces a server that an older build
started, ending its sessions, which then start again in the same
directories.

### Windows

GoRex runs on Windows 10 1809 or later, whose ConPTY runs the shells; its
data directory is `%AppData%\GoRex`. Panes run `$SHELL` when it names a
Windows program (`nu`, `C:\Program Files\Git\bin\bash.exe`), else
PowerShell (`pwsh`, then `powershell`), else `cmd`. PowerShell's `cd`
leaves its process's directory where it was, so GoRex starts it with a
prompt that sets it, wrapping the profile's own prompt: the header then
follows it, and new panes open there. The window is frameless and its
title bar draws minimize, maximize and close with the other buttons; it
resizes from its sides and bottom, not its top.

## Remote hosts

Add one from the host chip's popover (or Hosts ▸ Add Remote Host…) with an
ssh destination: `me@box`, or a `Host` of `~/.ssh/config`. GoRex runs
your `ssh`, so its config, keys, agent, `ProxyJump` and known hosts apply;
it runs it in `BatchMode`, as a window has no terminal to type a password
in, so the host must let you in with a key or an agent.

ssh runs `gorex -bridge` there, found in `~/.gorex/bin`, `~/.local/bin`,
`~/go/bin`, `/usr/local/bin` or the `PATH` (or as the host's "GoRex on the
host" says). The bridge starts that machine's server when none runs, and
carries every connection of the window to it, the control connection and
one per pane, over ssh's one pipe ([yamux](https://github.com/hashicorp/yamux)).
When the host has no GoRex and runs the same system on the same processor,
GoRex offers to install itself there (with libghostty-vt beside it); else
build it for the host and put it in `~/.gorex/bin`. A Linux server needs no
display: `-server` and `-bridge` never open a window.

What it does about latency and links that drop:

- **One round trip to a new pane.** The window chooses each session's ID
  and the server starts the session as the pane attaches, so a pane opens,
  and its shell's prompt shows, in a round trip, and no request waits on
  the window's thread.
- **Keystrokes go at once.** ssh holds small writes back (Nagle) without a
  terminal, so GoRex asks ssh for one (`-tt`) and the bridge makes it raw
  before saying a word; it checks that the pipe carries every byte as it
  is, and runs without one when it does not (as on Windows hosts). yamux's
  header goes in the same write as its data: one packet per keystroke.
- **Bounded output in flight.** Each pane's stream has a window of 256 KB,
  and the server drops a pane more than 2 MB behind: it attaches again and
  gets the screen as it is (with the last 2 MB of scrollback), as tmux and
  mosh skip what scrolled by. `yes` over a 200 ms link answers ^C within a
  second.
- **Reconnecting.** Pings every 4 seconds measure the latency (in the
  host's popover) and find a dead link in seconds; ssh's keepalives find
  it below. GoRex connects again with backoff, at once when the window
  comes back to the front (as after sleep), and the panes attach again to
  their sessions, their screens redrawn. While the link is down, panes dim
  and drop what is typed, which would otherwise run later, unseen; closing
  a pane or changing the layout meanwhile is sent once the link is back.
  Should the host's server have restarted (as the machine did), the panes
  start their shells again where they were. After 10 failed attempts in a
  row (about two minutes) GoRex gives up until you press Retry Now; set a
  host's "Reconnect attempts" when adding it (0 never gives up), or its
  `maxRetries` in `settings.json` (-1 never gives up).

Each host keeps its own layout, as this machine's: another window that
connects to it finds its tabs. Quit and End All Sessions ends this
machine's sessions only.

To try it against a host of yours from the tests:

```sh
GOREX_E2E_DEST=me@box go test -tags sshe2e -run 'TestSSH$' -v ./internal/rex/
```

`GOREX_E2E_COMMAND` names the GoRex there (as `env GOREX_DIR=/tmp/x
~/gorex`, to keep off the sessions of the GoRex in use there) and
`GOREX_E2E_ARGS` gives ssh more arguments.

## Layout

| | |
|---|---|
| `main.go` | the app, its window, `-server` and `-bridge` |
| `state.go` | tabs, the tree of splits, panes, saving and restoring the layout |
| `hosts.go` | the hosts: this machine and remote ones, connecting, switching, installing |
| `view.go`, `tabs.go`, `host.go`, `commands.go` | the interface: title bar, tabs, panes, host popover, menus and palette |
| `programs.go` | how programs show: names, glyphs, tile colors |
| `style.go`, `settings.go` | colors, fonts, terminal themes; appearance and text size |
| `internal/rex` | the session server and its client: PTYs, foreground processes, headless screens, the socket protocol |
| `internal/rex/remote.go`, `bridge.go` | remote hosts: ssh, the bridge, the pipe's connections |

Icons: [Lucide](https://lucide.dev) (ISC) and [Simple Icons](https://simpleicons.org)
(CC0). Font: [JetBrains Mono](https://www.jetbrains.com/lp/mono/) (OFL).
