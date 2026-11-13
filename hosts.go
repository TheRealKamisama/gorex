package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"gorex/internal/rex"
)

// Host is a machine whose session server the window shows the tabs of:
// this one, or one GoRex reaches through ssh (rex.Remote). Each has tabs
// of its own, whose layout its server keeps, so that they come back as
// they were from any window that connects to it.
type Host struct {
	// conf is a remote host's, as the user added it; remote reaches it.
	conf   hostConfig
	remote *rex.Remote
	client *rex.Client
	// hello is what its server said of itself, as it connected last.
	hello rex.Hello
	tabs  []*Tab
	// active is the index of the tab shown.
	active int
	// loaded tells that the tabs came from the server's layout, or were
	// made after it said it had none: until then, a layout saved would
	// overwrite the one it keeps.
	loaded       bool
	lastSnapshot string
	// The installation of GoRex on the host, while it goes on.
	installing  bool
	installDone float64
	installErr  string
}

// hostConfig is a remote host, as settings.json keeps it.
type hostConfig struct {
	ID string `json:"id"`
	// Name is the user's name of it: empty, the host's own.
	Name string `json:"name,omitempty"`
	// Destination is ssh's: [user@]host, or a Host of ~/.ssh/config.
	Destination string `json:"destination"`
	// Command runs GoRex there: empty, it is looked for.
	Command string   `json:"command,omitempty"`
	SSHArgs []string `json:"sshArgs,omitempty"`
	// MaxRetries is how many times in a row GoRex fails to reach the host
	// before it gives up, until told to retry: 0 is defaultMaxRetries, and
	// a negative number never gives up.
	MaxRetries int `json:"maxRetries,omitempty"`
}

// defaultMaxRetries is the attempts at reaching a host before giving up:
// about two minutes, with the waits between them growing.
const defaultMaxRetries = 10

// maxRetries is the client's MaxRetries of the host's configuration.
func (conf hostConfig) maxRetries() int {
	switch {
	case conf.MaxRetries < 0:
		return 0 // no limit
	case conf.MaxRetries == 0:
		return defaultMaxRetries
	}
	return conf.MaxRetries
}

func (h *Host) isRemote() bool { return h.remote != nil }

// id names the host in the settings: "" for this machine.
func (h *Host) id() string { return h.conf.ID }

// name is how the host shows.
func (h *Host) name() string {
	switch {
	case h.conf.Name != "":
		return h.conf.Name
	case h.hello.Host.Name != "":
		return h.hello.Host.Name
	case h.isRemote():
		_, host, ok := strings.Cut(h.conf.Destination, "@")
		if ok {
			return host
		}
		return h.conf.Destination
	}
	name, _ := os.Hostname()
	return strings.TrimSuffix(name, ".local")
}

// home is the home directory on the host, which directories show from
// as "~".
func (h *Host) home() string {
	if h.isRemote() {
		return h.hello.Host.Home
	}
	home, _ := os.UserHomeDir()
	return home
}

// status is how the host's client is connected.
func (h *Host) status() rex.Status {
	if h.client == nil {
		return rex.Status{State: rex.Stopped}
	}
	return h.client.Status()
}

// up tells that the host's server answers.
func (h *Host) up() bool { return h.client != nil && h.status().State == rex.Connected }

// ready tells that panes can open on the host: its tabs are loaded.
func (h *Host) ready() bool { return h.client != nil && h.loaded }

func (h *Host) tab() *Tab {
	if h.active >= 0 && h.active < len(h.tabs) {
		return h.tabs[h.active]
	}
	return nil
}

func (h *Host) panes() []*Pane {
	var ps []*Pane
	for _, t := range h.tabs {
		ps = append(ps, t.panes()...)
	}
	return ps
}

// newApp returns the app of a window on this machine's server.
func newApp(client *rex.Client) *App {
	h := &Host{client: client, loaded: true}
	return &App{hosts: []*Host{h}, host: h}
}

// local is this machine.
func (a *App) local() *Host { return a.hosts[0] }

// allPanes returns the panes of every host.
func (a *App) allPanes() []*Pane {
	var ps []*Pane
	for _, h := range a.hosts {
		ps = append(ps, h.panes()...)
	}
	return ps
}

// sshProgram is the ssh the remote hosts run: the system's when empty.
var sshProgram = ""

// remoteOf returns the Remote of a host's configuration.
func remoteOf(conf hostConfig) *rex.Remote {
	return &rex.Remote{Destination: conf.Destination, Command: conf.Command, Args: conf.SSHArgs, SSH: sshProgram}
}

// updater returns how goroutines change the state, on the main thread:
// in the window, or in the next frame of a view without one.
func (a *App) updater() func(func()) {
	if win := a.win; win != nil {
		return win.Update
	}
	svc := a.services
	return func(fn func()) {
		a.post(fn)
		svc.Invalidate()
	}
}

// openRemotes adds the hosts of the settings, and connects to them in the
// background.
func (a *App) openRemotes() {
	for _, conf := range prefs.Hosts {
		h := &Host{conf: conf, remote: remoteOf(conf)}
		a.hosts = append(a.hosts, h)
		a.connectHost(h)
		if conf.ID == prefs.ActiveHost {
			a.host = h
		}
	}
}

// Remote attachments get the last of the scrollback, and are dropped
// rather than fall seconds behind, on a slow link: attaching again, they
// get the screen as it is then.
const (
	remoteScrollback = 2 << 20
	remoteBacklog    = 2 << 20
)

// connectHost connects to a remote host, again and again, until it is
// disconnected, and asks its server what runs as the window's does.
func (a *App) connectHost(h *Host) {
	h.loaded, h.lastSnapshot = false, ""
	update := a.updater()
	c := rex.Dial(h.remote.Connector(), rex.DialOptions{
		Scrollback: remoteScrollback, Backlog: remoteBacklog,
		MaxRetries: h.conf.maxRetries(),
		// Draw the host's state as it changes.
		OnChange: func() { update(func() {}) },
	})
	h.client = c
	go a.pollRemote(update, h, c)
}

// pollRemote loads a remote host's tabs once it connects, then asks what
// runs in its sessions, as poll does on this machine, until the client
// closes, with the window or as the host disconnects.
func (a *App) pollRemote(update func(func()), h *Host, c *rex.Client) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	loaded, epoch := false, 0
	for {
		select {
		case <-c.Closed():
			return
		case <-tick.C:
		}
		if c.Status().State != rex.Connected {
			continue
		}
		if hello, e := c.Server(); e != epoch {
			epoch = e
			update(func() {
				if h.client == c {
					h.hello = hello
				}
			})
		}
		if !loaded {
			raw, err := c.Layout()
			if err != nil {
				continue
			}
			infos, err := c.List()
			if err != nil {
				continue
			}
			loaded = true
			update(func() {
				if h.client == c && !a.quitting {
					if !a.restoreHost(h, raw, infos) {
						a.newTabOn(h, "")
						if a.host != h {
							h.active = 0
						}
					}
				}
			})
			continue
		}
		infos, err := c.List()
		if err != nil {
			continue
		}
		byID := make(map[string]rex.SessionInfo, len(infos))
		for _, in := range infos {
			byID[in.ID] = in
		}
		update(func() {
			if h.client == c && !a.quitting {
				a.apply(h, byID)
			}
		})
	}
}

// disconnectHost closes the connection to a remote host, detaching its
// panes: their sessions go on there, and come back as it connects again.
func (a *App) disconnectHost(h *Host) {
	if !h.isRemote() || h.client == nil {
		return
	}
	if h.loaded {
		a.saveHost(h)
	}
	for _, p := range h.panes() {
		p.closed = true
		if p.term != nil {
			p.term.Close()
		}
	}
	h.tabs, h.active, h.loaded = nil, 0, false
	c := h.client
	h.client = nil
	go func() {
		time.Sleep(300 * time.Millisecond) // for the layout to go
		c.Close()
	}()
}

// reconnectHost connects to a host again: at once when it waits to, and
// anew when the link is up, as one that stopped answering.
func (a *App) reconnectHost(h *Host) {
	if !h.isRemote() {
		return
	}
	if h.client == nil {
		a.connectHost(h)
		return
	}
	h.client.Reconnect()
}

// selectHost shows a host's tabs in the window.
func (a *App) selectHost(h *Host) {
	if h == a.host {
		return
	}
	a.host = h
	a.paletteOpen, a.renaming = false, nil
	if h.isRemote() && h.client == nil {
		a.reconnectHost(h)
	}
	if h.ready() && len(h.tabs) == 0 {
		a.newTabOn(h, "")
	}
	if t := h.tab(); t != nil {
		a.focusReq = t.Focus
	}
	prefs.ActiveHost = h.id()
	saveSettings()
	a.changed()
}

// addHost adds a remote host, connects to it and shows it.
func (a *App) addHost(conf hostConfig) error {
	conf.Destination = strings.TrimSpace(conf.Destination)
	conf.Name = strings.TrimSpace(conf.Name)
	conf.Command = strings.TrimSpace(conf.Command)
	r := remoteOf(conf)
	if err := r.Valid(); err != nil {
		return err
	}
	for _, h := range a.hosts {
		if h.isRemote() && h.conf.Destination == conf.Destination && h.conf.Command == conf.Command {
			a.selectHost(h)
			return nil
		}
	}
	conf.ID = rex.NewID()
	h := &Host{conf: conf, remote: r}
	a.hosts = append(a.hosts, h)
	prefs.Hosts = append(prefs.Hosts, conf)
	saveSettings()
	a.connectHost(h)
	a.selectHost(h)
	return nil
}

// removeHost forgets a remote host; its sessions go on there.
func (a *App) removeHost(h *Host) {
	if !h.isRemote() {
		return
	}
	a.disconnectHost(h)
	a.hosts = slices.DeleteFunc(a.hosts, func(o *Host) bool { return o == h })
	prefs.Hosts = slices.DeleteFunc(prefs.Hosts, func(c hostConfig) bool { return c.ID == h.conf.ID })
	if a.host == h {
		a.host = a.local()
		if t := a.host.tab(); t != nil {
			a.focusReq = t.Focus
		} else if a.host.ready() {
			a.newTab(a.currentDir())
		}
	}
	prefs.ActiveHost = a.host.id()
	saveSettings()
	a.changed()
}

// installOn copies GoRex to a host that has none, then connects.
func (a *App) installOn(h *Host) {
	if !h.isRemote() || h.installing {
		return
	}
	update, r := a.updater(), *h.remote
	h.installing, h.installDone, h.installErr = true, 0, ""
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		last := time.Time{}
		err := r.Install(ctx, func(sent, total int64) {
			if time.Since(last) < 100*time.Millisecond {
				return
			}
			last = time.Now()
			update(func() { h.installDone = float64(sent) / float64(max(total, 1)) })
		})
		update(func() {
			h.installing = false
			if err != nil {
				log.Printf("installing on %s: %v", h.conf.Destination, err)
				h.installErr = err.Error()
				return
			}
			a.reconnectHost(h)
		})
	}()
}

// hostProblem describes why a host is out of reach, if it is.
func hostProblem(h *Host) (msg string, missing *rex.MissingError) {
	st := h.status()
	if st.Err == nil {
		return "", nil
	}
	errors.As(st.Err, &missing)
	return st.Err.Error(), missing
}

// statusLine says how a host is connected, in a few words.
func (h *Host) statusLine(now time.Time) string {
	if !h.isRemote() {
		return thisMachine() + " · connected"
	}
	if h.client == nil {
		return "Disconnected"
	}
	st := h.status()
	switch st.State {
	case rex.Connected:
		if st.Stalled {
			return "Not answering…"
		}
		if !h.loaded {
			return "Loading…"
		}
		if st.RTT > 0 {
			return fmt.Sprintf("Connected · %s", roundRTT(st.RTT))
		}
		return "Connected"
	case rex.Connecting:
		if st.Attempts > 0 {
			return "Reconnecting…"
		}
		return "Connecting…"
	case rex.GaveUp:
		return fmt.Sprintf("Gave up after %d attempts", st.Attempts)
	case rex.Waiting:
		if d := st.RetryAt.Sub(now); d > time.Second {
			return fmt.Sprintf("Retrying in %ds", int(d.Seconds()+0.5))
		}
		return "Retrying…"
	}
	return "Disconnected"
}

func roundRTT(d time.Duration) string {
	if d < 10*time.Millisecond {
		return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

// saveHost sends a host's layout to its server, when it changed.
func (a *App) saveHost(h *Host) {
	if !h.loaded || h.client == nil {
		return
	}
	b, err := json.Marshal(a.snapshotOf(h))
	if err != nil || string(b) == h.lastSnapshot {
		return
	}
	h.lastSnapshot = string(b)
	a.lastSave = time.Now()
	c := h.client
	go c.SetLayout(b)
}
