// GoRex is a terminal of tabs and split panes, after Superlogical's Rex,
// in MyGo's native UI. Its shells run in a session server of their own,
// so that they outlive the app: quit it, open it again, and every tab and
// pane is back as it was, its programs still running.
//
//	go tool mygo dev    the app, rebuilt as the code changes
//	go run . -server    the session server alone, in the foreground
//	gorex -bridge       what ssh runs on a remote host (remote.go)
package main

import (
	"encoding/json"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

func main() {
	// -server runs the session server; other arguments are AppKit's, as
	// -NSWindowResizeTime, which it reads itself.
	if len(os.Args) > 1 && os.Args[1] == "-server" {
		log.SetPrefix("[server] ")
		if err := rex.Serve(); err != nil {
			log.Fatal(err)
		}
		return
	}
	// -bridge serves a GoRex on another machine, which ssh ran it for.
	if len(os.Args) > 1 && os.Args[1] == "-bridge" {
		if err := rex.Bridge(); err != nil {
			os.Exit(1)
		}
		return
	}
	// The state of the sessions lives in the app's data directory, which
	// the server it starts takes from GOREX_DIR: "GoRex", or "GoRex Dev"
	// under mygo dev, which keeps the installed app's sessions apart.
	if os.Getenv("GOREX_DIR") == "" {
		if dir, err := mygo.App.Path(mygo.PathUserData); err == nil {
			os.Setenv("GOREX_DIR", dir)
		}
	}
	registerFonts()
	loadSettings()
	a := newApp(nil)
	mygo.App.SetMenu(a.menu())
	mygo.App.WhenReady(a.open)
	mygo.App.OnActivate(func(hasVisibleWindows bool) {
		if !hasVisibleWindows && a.win == nil {
			a.open()
		}
	})
	mygo.App.OnWindowAllClosed(func() {
		if runtime.GOOS != "darwin" {
			mygo.App.Quit()
		}
	})
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) {
		a.saveNow()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// open opens the window, with the tabs the server kept.
func (a *App) open() {
	client, err := rex.Connect()
	if err != nil {
		log.Print(err)
		mygo.Dialog.Error("GoRex could not start its session server", err.Error())
		mygo.App.Quit()
		return
	}
	h := &Host{client: client, loaded: true}
	a.hosts, a.host = []*Host{h}, h
	h.hello, err = client.Hello()
	if err == nil && mygo.IsDev() && staleServer(h.hello) {
		// A server a build before this one started runs that build's
		// code: start one of this build's, ending its sessions.
		if client, err = client.Restart(h.hello.PID); err == nil {
			h.client = client
			h.hello, err = client.Hello()
		} else {
			log.Print(err)
			mygo.Dialog.Error("GoRex could not restart its session server", err.Error())
			mygo.App.Quit()
			return
		}
	}
	if err == nil && h.hello.Version != rex.ProtocolVersion {
		a.err = "The session server is of another version of GoRex: quit and end all sessions to restart it."
	}
	a.reset()
	opts := mygo.WindowOptions{
		Title:                "GoRex",
		Width:                1000,
		Height:               620,
		MinWidth:             560,
		MinHeight:            340,
		StateKey:             "main",
		TitleBarStyle:        mygo.TitleBarHidden,
		TrafficLightPosition: &mygo.Point{X: 16, Y: 15},
		BackgroundColor:      "light-dark(#efe1e6, #231e27)",
		Content:              ui.View(a.view),
	}
	if ownControls {
		// The title bar draws the window's controls (windowControls).
		opts.TitleBarStyle, opts.Frameless = mygo.TitleBarDefault, true
	}
	win := mygo.NewWindow(opts)
	a.win = win
	if !a.restore() {
		home, _ := os.UserHomeDir()
		a.newTabOn(h, home)
	}
	// The remote hosts connect in the background: their tabs show as
	// they load.
	a.openRemotes()
	a.changed()
	go a.poll(win, h)
	a.debugHook()
	win.OnClosed(func() {
		a.saveNow()
		a.quitting = true
		for _, p := range a.allPanes() {
			p.closed = true
			if p.term != nil {
				p.term.Close() // detaches: the session goes on
			}
		}
		for _, h := range a.hosts {
			if h.client != nil {
				h.client.Close()
			}
		}
		a.win = nil
	})
}

// saveNow sends the layouts to the servers and waits a moment for them
// to be kept.
func (a *App) saveNow() {
	done := make(chan struct{}, len(a.hosts))
	n := 0
	for _, h := range a.hosts {
		if h.client == nil || !h.loaded || len(h.tabs) == 0 && !a.quitting {
			continue
		}
		b, err := json.Marshal(a.snapshotOf(h))
		if err != nil {
			continue
		}
		h.lastSnapshot = string(b)
		c := h.client
		n++
		go func() {
			c.SetLayout(b)
			done <- struct{}{}
		}()
	}
	timeout := time.After(time.Second)
	for range n {
		select {
		case <-done:
		case <-timeout:
			return
		}
	}
}

// reset forgets the state of a window that closed, for the next window.
func (a *App) reset() {
	a.focusReq = nil
	a.paletteOpen, a.hostOpen, a.renaming, a.addOpen = false, false, nil, false
	a.saveDue, a.quitting, a.title = false, false, ""
}

// staleServer reports whether the server runs another build of the app
// than this one, as after mygo dev rebuilt it.
func staleServer(h rex.Hello) bool {
	exe, at := rex.Executable()
	return h.Exe != exe || !h.ExeTime.Equal(at)
}
