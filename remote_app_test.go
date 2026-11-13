//go:build darwin || linux

package main

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gorex/internal/rex"
)

// The test binary also plays ssh, running the bridge on a "remote"
// GoRex directory, and the servers the bridge starts.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-server" {
		if err := rex.Serve(); err != nil {
			os.Exit(1)
		}
		return
	}
	if os.Getenv("GOREX_TEST_ROLE") == "ssh" {
		os.Setenv("GOREX_TEST_ROLE", "")
		os.Setenv("GOREX_DIR", os.Getenv("GOREX_TEST_REMOTE_DIR"))
		os.Stdout.WriteString("Welcome!\n")
		if os.Getenv("GOREX_TEST_FAIL") != "" {
			os.Stderr.WriteString("ssh: connect to host testhost port 22: Connection refused\r\n")
			os.Exit(255)
		}
		if os.Getenv("GOREX_TEST_MISSING") != "" {
			os.Stdout.WriteString("GOREX-MISSING Linux x86_64\n")
			os.Exit(127)
		}
		if rex.Bridge() != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestRemoteHost(t *testing.T) {
	a, tt := newTestApp(t)
	remoteDir := testDir(t)
	exe, _ := os.Executable()
	t.Setenv("GOREX_TEST_ROLE", "ssh")
	t.Setenv("GOREX_TEST_REMOTE_DIR", remoteDir)
	sshProgram = exe
	prefs = settings{FontSize: defaultFontSize}
	defer func() { sshProgram = "" }()
	local := a.host
	if err := a.addHost(hostConfig{Destination: "-bad"}); err == nil {
		t.Error("a host of a dash")
	}
	if err := a.addHost(hostConfig{Destination: "testhost", Name: "Box"}); err != nil {
		t.Fatal(err)
	}
	h := a.host
	if !h.isRemote() || h.name() != "Box" || len(prefs.Hosts) != 1 || prefs.ActiveHost != h.id() {
		t.Fatalf("host %+v, prefs %+v", h.conf, prefs)
	}
	t.Cleanup(func() {
		if pid := h.hello.PID; pid > 0 {
			syscall.Kill(pid, syscall.SIGTERM)
		}
		if h.client != nil {
			h.client.Close()
		}
		os.RemoveAll(remoteDir)
	})

	// Until it connects, the window shows the host's state; then a tab of
	// a shell there.
	tt.Frame()
	waitFor(t, tt, "the remote shell", func() bool {
		tab := a.tab()
		return tab != nil && tab.Focus.term != nil && strings.Contains(tab.Focus.term.Text(), testShell.prompt)
	})
	if !strings.Contains(strings.Join(tt.Texts(), " "), "Box") {
		t.Errorf("the chip does not name the host: %q", tt.Texts())
	}
	tab := a.tab()
	if tab.host != h || tab.Focus.host != h {
		t.Fatal("the tab is not the remote host's")
	}
	a.split(false)
	right := a.tab().Focus
	waitFor(t, tt, "the second remote shell", func() bool {
		return right.term != nil && strings.Contains(right.term.Text(), testShell.prompt)
	})
	right.term.Send([]byte(testShell.run + "\r"))
	waitFor(t, tt, testShell.program+" in the header", func() bool {
		name, _ := right.label()
		return strings.EqualFold(name, testShell.program)
	})

	// The link drops: the panes show it, then attach again to their
	// sessions.
	_, epoch := h.client.Server()
	h.client.Reconnect()
	waitFor(t, tt, "the link to come back", func() bool {
		_, e := h.client.Server()
		return e > epoch && h.up() && right.stream.Attached()
	})
	right.term.Send([]byte("echo ba''ck\r"))
	waitFor(t, tt, "the shell to answer again", func() bool { return strings.Contains(right.term.Text(), "back") })

	// This computer's tabs are as they were.
	a.selectHost(local)
	tt.Frame()
	if a.tab() == nil || a.tab().host != local {
		t.Fatal("this computer's tabs are not shown")
	}
	a.selectHost(h)
	tt.Frame()

	// The layout is the host's: a window that connects to it anew (as
	// the next launch) finds its tabs and sessions.
	a.saveNow()
	sids := map[string]bool{}
	for _, p := range h.panes() {
		sids[p.SID] = true
	}
	a.disconnectHost(h)
	if len(h.tabs) != 0 || h.client != nil {
		t.Fatal("the host's tabs stay")
	}
	tt.Frame()
	a.reconnectHost(h)
	waitFor(t, tt, "the tabs to come back", func() bool { return len(h.panes()) == 2 })
	for _, p := range h.panes() {
		if !sids[p.SID] {
			t.Errorf("pane of session %q, not one before", p.SID)
		}
	}

	// Closing the last tab of the host shows this computer's.
	for len(h.tabs) > 0 {
		a.closeTab(h.tabs[0])
	}
	if a.host != local {
		t.Error("the window still shows the remote host")
	}
	a.removeHost(h)
	if len(a.hosts) != 1 || len(prefs.Hosts) != 0 {
		t.Errorf("hosts left %d %+v", len(a.hosts), prefs.Hosts)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestShortDirIn(t *testing.T) {
	if d := shortDirIn("/home/me/src/x", "/home/me"); d != "~/src/x" {
		t.Errorf("remote dir %q", d)
	}
	if d := shortDirIn("/home/me", "/home/me"); d != "~" {
		t.Errorf("remote home %q", d)
	}
	if d := shortDirIn("/home/meow", "/home/me"); d != "/home/meow" {
		t.Errorf("another home %q", d)
	}
}

// TestShots draws the remote hosts' interface into GOREX_SHOTS, to look
// at: the host's popover, the dialog that adds one, a host without GoRex,
// and the panes of a host out of reach.
func TestShots(t *testing.T) {
	dir := os.Getenv("GOREX_SHOTS")
	if dir == "" {
		t.Skip("GOREX_SHOTS names no directory")
	}
	a, tt := newTestApp(t)
	tt.SetDark(os.Getenv("GOREX_SHOTS_DARK") != "")
	remoteDir := testDir(t)
	exe, _ := os.Executable()
	t.Setenv("GOREX_TEST_ROLE", "ssh")
	t.Setenv("GOREX_TEST_REMOTE_DIR", remoteDir)
	sshProgram = exe
	prefs = settings{FontSize: defaultFontSize}
	defer func() { sshProgram = "" }()
	save := func(name string) {
		tt.Frame()
		time.Sleep(50 * time.Millisecond)
		tt.Frame()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, tt.Image())
		f.Close()
	}
	if err := a.addHost(hostConfig{Destination: "me@build-box"}); err != nil {
		t.Fatal(err)
	}
	h := a.host
	t.Cleanup(func() {
		if pid := h.hello.PID; pid > 0 {
			syscall.Kill(pid, syscall.SIGTERM)
		}
		if h.client != nil {
			h.client.Close()
		}
		os.RemoveAll(remoteDir)
	})
	waitFor(t, tt, "the remote shell", func() bool {
		tab := a.tab()
		return tab != nil && tab.Focus.term != nil && strings.Contains(tab.Focus.term.Text(), testShell.prompt)
	})
	a.split(false)
	time.Sleep(5 * time.Second) // a ping or two
	a.hostOpen = true
	save("1-popover.png")
	a.hostOpen = false
	a.openAddHost()
	a.addDest = "me@gpu-box"
	save("2-add.png")
	a.addOpen = false

	// The link drops, and ssh fails: the panes wait.
	t.Setenv("GOREX_TEST_FAIL", "1")
	h.client.Reconnect()
	waitFor(t, tt, "the link to fail", func() bool { return h.status().State == rex.Waiting })
	save("3-offline.png")

	// A host without GoRex.
	t.Setenv("GOREX_TEST_FAIL", "")
	t.Setenv("GOREX_TEST_MISSING", "1")
	if err := a.addHost(hostConfig{Destination: "me@new-box"}); err != nil {
		t.Fatal(err)
	}
	m := a.host
	defer m.client.Close()
	waitFor(t, tt, "the host to lack GoRex", func() bool { _, missing := hostProblem(m); return missing != nil })
	save("4-missing.png")
}

// A host out of reach shows that GoRex gave up after its attempts, and
// Retry Now tries it again.
func TestRemoteGivesUp(t *testing.T) {
	a, tt := newTestApp(t)
	remoteDir := testDir(t)
	exe, _ := os.Executable()
	t.Setenv("GOREX_TEST_ROLE", "ssh")
	t.Setenv("GOREX_TEST_REMOTE_DIR", remoteDir)
	t.Setenv("GOREX_TEST_FAIL", "1")
	sshProgram = exe
	prefs = settings{FontSize: defaultFontSize}
	defer func() { sshProgram = "" }()
	if err := a.addHost(hostConfig{Destination: "testhost", MaxRetries: 2}); err != nil {
		t.Fatal(err)
	}
	h := a.host
	t.Cleanup(func() {
		if pid := h.hello.PID; pid > 0 {
			syscall.Kill(pid, syscall.SIGTERM)
		}
		h.client.Close()
		os.RemoveAll(remoteDir)
	})
	waitFor(t, tt, "GoRex to give up", func() bool { return h.status().State == rex.GaveUp })
	tt.Frame()
	if !tt.HasText("Gave up after 2 attempts") || !tt.HasText("Retry Now") {
		t.Errorf("texts %q", tt.Texts())
	}
	os.Setenv("GOREX_TEST_FAIL", "")
	if err := tt.Click("Retry Now"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, tt, "a tab of the host", func() bool { return a.tab() != nil && a.tab().host == h })
}

func TestParseRetries(t *testing.T) {
	for in, want := range map[string]int{"": 0, " 5 ": 5, "0": -1} {
		if got, err := parseRetries(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"-1", "x"} {
		if _, err := parseRetries(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
	if (hostConfig{}).maxRetries() != defaultMaxRetries || (hostConfig{MaxRetries: -1}).maxRetries() != 0 {
		t.Error("maxRetries")
	}
}
