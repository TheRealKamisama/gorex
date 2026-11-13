package main

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

// errorRed is the dot of a host out of reach.
var errorRed = ui.Hex("#ef4444")

// hostGlyph is the icon of a host: by its model.
func hostGlyph(h *Host) string {
	model := h.hello.Host.Model
	switch {
	case strings.Contains(model, "MacBook"), h.hello.Host.Portable:
		return "laptop"
	case strings.Contains(model, "iMac"):
		return "monitor"
	case h.isRemote():
		return "server"
	case runtime.GOOS == "windows":
		return "monitor"
	case model == "Linux":
		return "server"
	}
	return "mac-studio"
}

// hostDot is the color of a host's state: green connected, amber on the
// way or slow, red out of reach.
func hostDot(h *Host, k *colors) ui.Color {
	if !h.isRemote() {
		return k.busy
	}
	st := h.status()
	switch {
	case h.client == nil:
		return k.textFaint
	case st.State == rex.Connected && (st.Stalled || !h.loaded):
		return k.attention
	case st.State == rex.Connected:
		return k.busy
	case st.State == rex.Waiting && st.Err != nil, st.State == rex.GaveUp:
		return errorRed
	}
	return k.attention
}

// hostChip shows the machine the sessions of the window run on; a click
// shows more of it, the other hosts, and adds one.
func (a *App) hostChip(c *ui.Context, k *colors) {
	h := a.host
	name, model := h.name(), h.hello.Host.Model
	if h.isRemote() && (!h.up() || model == "") {
		model = h.statusLine(c.Now())
		c.After(time.Second) // a countdown
	}
	glyph := hostGlyph(h)
	chip := ui.Row(c).Gap(9).Padding(3, 8, 3, 6).Radius(9).AlignItems(ui.Center).Cursor(ui.CursorPointer).
		Role(ui.RoleButton).Label("Host " + name).MaxWidth(190).Shrink(0)
	if chip.Hovered() || a.hostOpen {
		chip.Background(k.hover)
	}
	if chip.Clicked() {
		a.hostOpen = !a.hostOpen
	}
	chip.Children(func() {
		ui.Box(c).Size(19, 19).Children(func() {
			ui.Icon(c, icon(glyph)).Size(19, 19).TextColor(k.textMuted)
			if h.isRemote() {
				ui.Box(c).Absolute().Right(-2).Bottom(-1).Size(8, 8).Radius(4).Background(hostDot(h, k)).Border(1.5, k.bgBottom)
			}
		})
		ui.Column(c).MinWidth(0).Children(func() {
			ui.Text(c, name).FontSize(13.5).FontWeight(700).TextColor(k.text.Alpha(0.85)).SingleLine().Ellipsis("…")
			ui.Text(c, model).FontSize(11.5).TextColor(k.textFaint).SingleLine().Ellipsis("…")
		})
	})
	ui.PopoverBase(c, chip, &a.hostOpen, func(panel ui.Element) {
		panel.Margin(8, 0, 0, 0).Width(320).Padding(14).Radius(14).Background(k.panel).Border(1, k.panelBorder).
			Shadow(0, 12, 32, 0, k.shadowFocused).Gap(12)
		a.hostPanel(c, k, h)
	})
}

// smallButton is a button of text, as the host panel's.
func smallButton(c *ui.Context, k *colors, label string, primary bool) ui.Element {
	b := ui.Box(c).Padding(5, 11).Radius(8).Cursor(ui.CursorPointer).Role(ui.RoleButton).Label(label).Shrink(0)
	bg, fg := k.hover, k.text
	if primary {
		bg, fg = ui.Hex("#2e6fd0"), ui.Hex("#ffffff")
	}
	if b.Pressed() {
		bg = bg.Alpha(0.7)
	} else if b.Hovered() {
		bg = bg.Alpha(0.85)
	}
	b.Background(bg).Transition(ui.ElementTransition{Colors: true, Duration: 120 * time.Millisecond})
	b.Children(func() {
		ui.Text(c, label).FontSize(12.5).FontWeight(600).TextColor(fg).SingleLine()
	})
	return b
}

func (a *App) hostPanel(c *ui.Context, k *colors, h *Host) {
	info := h.hello.Host
	name, glyph := h.name(), hostGlyph(h)
	now := c.Now()
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(38, 38).Radius(10).Background(k.hover).Center().Children(func() {
			ui.Icon(c, icon(glyph)).Size(24, 24).TextColor(k.text)
		})
		ui.Column(c).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, name).FontSize(14).FontWeight(700).TextColor(k.text).SingleLine().Ellipsis("…")
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(7, 7).Radius(4).Background(hostDot(h, k))
				ui.Text(c, h.statusLine(now)).FontSize(12).TextColor(k.textFaint).SingleLine().Ellipsis("…")
			})
		})
	})
	if msg, missing := hostProblem(h); msg != "" && !h.up() {
		ui.Text(c, msg).FontSize(12).TextColor(k.text).LineHeight(1.35)
		if missing != nil && missing.CanInstall() {
			a.installRow(c, k, h)
		}
	}
	ui.Divider(c)
	row := func(label, value string) {
		if value == "" {
			return
		}
		ui.Row(c).Gap(12).Children(func() {
			ui.Text(c, label).FontSize(12.5).TextColor(k.textFaint).Width(78)
			ui.Text(c, value).FontSize(12.5).TextColor(k.text).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		})
	}
	if info.Model != "" || info.OS != "" {
		ui.Column(c).Gap(7).Children(func() {
			row("Model", info.Model)
			row("Chip", info.Chip)
			if info.Memory > 0 {
				row("Memory", fmt.Sprintf("%d GB", info.Memory>>30))
			}
			row("System", info.OS)
			row("User", info.User)
		})
		ui.Divider(c)
	}
	sessions, running := 0, 0
	for _, p := range h.panes() {
		sessions++
		if !p.info.Idle && p.info.Program != "" {
			running++
		}
	}
	ui.Column(c).Gap(7).Children(func() {
		if h.isRemote() {
			row("Via", "ssh "+h.conf.Destination)
			if st := h.status(); st.RTT > 0 && h.up() {
				row("Latency", roundRTT(st.RTT))
			}
		}
		if h.hello.PID > 0 {
			row("Server", fmt.Sprintf("pid %d · up %s", h.hello.PID, roundDur(time.Since(h.hello.Started))))
		}
		row("Sessions", fmt.Sprintf("%d open, %d running a program", sessions, running))
	})
	if h.isRemote() {
		ui.Row(c).Gap(6).Children(func() {
			if h.client == nil {
				if smallButton(c, k, "Connect", true).Clicked() {
					a.reconnectHost(h)
				}
			} else {
				if st := h.status(); st.State != rex.Connected && smallButton(c, k, "Retry Now", true).Clicked() {
					a.reconnectHost(h)
				}
				if smallButton(c, k, "Disconnect", false).Clicked() {
					a.later(c, func() { a.disconnectHost(h) })
				}
			}
			ui.Spacer(c)
			if smallButton(c, k, "Remove", false).Tooltip("Forget this host; its sessions go on there").Clicked() {
				a.later(c, func() { a.removeHost(h) })
				a.hostOpen = false
			}
		})
		ui.Text(c, "Sessions live in the host's server: losing the link loses nothing, and GoRex attaches to them again as it reconnects.").
			FontSize(11.5).TextColor(k.textFaint).LineHeight(1.35)
	} else {
		ui.Text(c, "Sessions live in the server: quit GoRex and they keep running, and come back as they were when it opens again.").
			FontSize(11.5).TextColor(k.textFaint).LineHeight(1.35)
	}
	ui.Divider(c)
	ui.Text(c, "HOSTS").FontSize(10.5).FontWeight(700).TextColor(k.textFaint)
	ui.Column(c).Gap(2).Margin(-6, -6, 0, -6).Children(func() {
		for i, o := range a.hosts {
			o := o
			r := ui.Row(c.Key(i)).Height(32).Padding(0, 8).Gap(9).Radius(8).AlignItems(ui.Center).Cursor(ui.CursorPointer).
				Role(ui.RoleButton).Label("Show " + o.name())
			if o == h {
				r.Background(k.panelSel)
			} else if r.Hovered() {
				r.Background(k.hover)
			}
			if r.Clicked() && o != h {
				a.hostOpen = false
				a.later(c, func() { a.selectHost(o) })
			}
			r.Children(func() {
				ui.Icon(c, icon(hostGlyph(o))).Size(15, 15).TextColor(k.textMuted)
				ui.Text(c, o.name()).FontSize(13).FontWeight(500).TextColor(k.text).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				ui.Spacer(c)
				detail := o.statusLine(now)
				if !o.isRemote() {
					detail = thisMachine()
				} else if st := o.status(); o.up() && st.RTT > 0 && !st.Stalled {
					detail = roundRTT(st.RTT)
				}
				ui.Text(c, detail).FontSize(11.5).TextColor(k.textFaint).SingleLine().Shrink(0)
				ui.Box(c).Size(7, 7).Radius(4).Background(hostDot(o, k)).Shrink(0)
			})
		}
		add := ui.Row(c.Key("add")).Height(32).Padding(0, 8).Gap(9).Radius(8).AlignItems(ui.Center).Cursor(ui.CursorPointer).
			Role(ui.RoleButton).Label("Add Remote Host")
		if add.Hovered() {
			add.Background(k.hover)
		}
		if add.Clicked() {
			a.hostOpen = false
			a.later(c, a.openAddHost)
		}
		add.Children(func() {
			ui.Icon(c, icon("plus")).Size(15, 15).TextColor(k.textMuted)
			ui.Text(c, "Add Remote Host…").FontSize(13).FontWeight(500).TextColor(k.text)
		})
	})
}

// installRow offers to install GoRex on a host that has none, and shows
// how it goes.
func (a *App) installRow(c *ui.Context, k *colors, h *Host) {
	switch {
	case h.installing:
		ui.Column(c).Gap(6).Children(func() {
			ui.Text(c, fmt.Sprintf("Installing GoRex on %s… %d%%", h.name(), int(h.installDone*100))).FontSize(12).TextColor(k.textFaint)
			ui.Progress(c, h.installDone)
		})
	default:
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if smallButton(c, k, "Install GoRex on Host", true).Tooltip("Copies this GoRex into ~/.gorex/bin there").Clicked() {
				a.installOn(h)
			}
			if h.installErr != "" {
				ui.Text(c, h.installErr).FontSize(11.5).TextColor(errorRed).Grow(1).MinWidth(0)
			}
		})
	}
}

// hostPlaceholder fills the window of a host without tabs: one that
// connects, or is out of reach.
func (a *App) hostPlaceholder(c *ui.Context, k *colors) {
	h := a.host
	now := c.Now()
	c.After(time.Second)
	ui.Column(c).Grow(1).Center().Children(func() {
		ui.Column(c).Width(380).Padding(22).Gap(12).Radius(cardR+4).Background(k.card).Border(1, k.cardBorder).
			Shadow(0, 6, 22, -2, k.shadow).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon(hostGlyph(h))).Size(30, 30).TextColor(k.textMuted)
			ui.Text(c, h.name()).FontSize(15).FontWeight(700).TextColor(k.text)
			st := h.status()
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				if h.client != nil && (st.State == rex.Connecting || st.State == rex.Connected) {
					ui.Spinner(c).Size(13, 13)
				} else {
					ui.Box(c).Size(7, 7).Radius(4).Background(hostDot(h, k))
				}
				ui.Text(c, h.statusLine(now)).FontSize(12.5).TextColor(k.textFaint)
			})
			msg, missing := hostProblem(h)
			if msg != "" && !h.up() {
				ui.Text(c, msg).FontSize(12.5).TextColor(k.text).LineHeight(1.35).TextAlign(ui.Center)
			}
			if missing != nil && !missing.CanInstall() {
				ui.Text(c, "Build GoRex for "+missing.OS+" "+missing.Arch+" and put it in ~/.gorex/bin/gorex there, with libghostty-vt beside it.").
					FontSize(11.5).TextColor(k.textFaint).LineHeight(1.35).TextAlign(ui.Center)
			}
			if missing != nil && missing.CanInstall() {
				a.installRow(c, k, h)
			}
			ui.Row(c).Gap(8).Margin(4, 0, 0, 0).Children(func() {
				switch {
				case h.client == nil:
					if smallButton(c, k, "Connect", true).Clicked() {
						a.reconnectHost(h)
					}
				case st.State == rex.Waiting, st.State == rex.GaveUp:
					if smallButton(c, k, "Retry Now", missing == nil || !missing.CanInstall()).Clicked() {
						a.reconnectHost(h)
					}
				case h.ready():
					if smallButton(c, k, "New Tab", true).Clicked() {
						a.newTab(a.currentDir())
					}
				}
				if smallButton(c, k, "Show "+a.local().name(), false).Clicked() {
					a.later(c, func() { a.selectHost(a.local()) })
				}
			})
		})
	})
}

// openAddHost opens the dialog that adds a remote host.
func (a *App) openAddHost() {
	a.addOpen, a.addDest, a.addName, a.addCmd, a.addRetries, a.addErr, a.addFocus = true, "", "", "", "", "", false
	a.hostOpen, a.paletteOpen = false, false
	a.focusReq = nil // the dialog takes the focus
}

// addHostDialog asks for the ssh destination of a host, and adds it.
func (a *App) addHostDialog(c *ui.Context, k *colors) {
	if !a.addOpen {
		return
	}
	was := a.addOpen
	submit := func() {
		retries, err := parseRetries(a.addRetries)
		if err != nil {
			a.addErr = err.Error()
			return
		}
		if err := a.addHost(hostConfig{Destination: a.addDest, Name: a.addName, Command: a.addCmd, MaxRetries: retries}); err != nil {
			a.addErr = err.Error()
			return
		}
		a.addOpen = false
	}
	ui.DialogBase(c, &a.addOpen, func(backdrop, panel ui.Element) {
		backdrop.Background(k.backdrop).Justify(ui.Start).Padding(78, 0, 0, 0)
		panel.Width(460).Radius(16).Background(k.panel).Border(1, k.panelBorder).Padding(18).Gap(12).
			Shadow(0, 24, 60, -8, ui.RGBA(0, 0, 0, 0.28)).Shadow(0, 2, 6, 0, ui.RGBA(0, 0, 0, 0.06))
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon("server")).Size(18, 18).TextColor(k.textMuted)
			ui.Text(c, "Add Remote Host").FontSize(15).FontWeight(700).TextColor(k.text)
		})
		ui.Text(c, "GoRex reaches the host with your ssh: its config, keys and agent. The host must let you in without a password, and run GoRex, which GoRex can install there when it runs the same system.").
			FontSize(12).TextColor(k.textFaint).LineHeight(1.35)
		field := func(label, placeholder string, value *string, focus bool) {
			ui.Column(c).Gap(5).Children(func() {
				ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(k.textMuted)
				box := ui.Row(c).Padding(7, 10).Radius(8).Background(k.hover).Border(1, k.panelBorder)
				box.Children(func() {
					in := ui.TextInputBase(c, value).Grow(1).FontSize(13.5).Placeholder(placeholder)
					if focus && !a.addFocus {
						in.Focus()
						a.addFocus = true
					}
					if in.Changed() {
						a.addErr = ""
					}
					if in.Submitted() {
						submit()
					}
				})
			})
		}
		field("SSH destination", "user@example.com, or a Host of ~/.ssh/config", &a.addDest, true)
		field("Name", "Optional: the host's own name when empty", &a.addName, false)
		field("GoRex on the host", "Optional: found in ~/.gorex/bin, ~/.local/bin or the PATH", &a.addCmd, false)
		field("Reconnect attempts", fmt.Sprintf("Optional: %d, then GoRex waits for Retry; 0 never gives up", defaultMaxRetries), &a.addRetries, false)
		if a.addErr != "" {
			ui.Text(c, a.addErr).FontSize(12).TextColor(errorRed)
		}
		ui.Row(c).Gap(8).Justify(ui.End).Margin(4, 0, 0, 0).Children(func() {
			if smallButton(c, k, "Cancel", false).Clicked() {
				a.addOpen = false
			}
			if smallButton(c, k, "Add Host", true).Clicked() {
				submit()
			}
		})
	})
	if was && !a.addOpen {
		if t := a.tab(); t != nil && a.focusReq == nil {
			a.focusReq = t.Focus
		}
	}
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// thisMachine names the machine the app runs on, as its system does.
func thisMachine() string {
	switch runtime.GOOS {
	case "darwin":
		return "This Mac"
	case "windows":
		return "This PC"
	}
	return "This computer"
}

// parseRetries reads the attempts of the dialog: empty is the default, 0
// is no limit (-1 in the settings).
func parseRetries(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("reconnect attempts: a number, 0 for no limit")
	}
	if n == 0 {
		return -1, nil
	}
	return n, nil
}
