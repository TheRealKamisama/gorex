//go:build sshe2e && (darwin || linux)

package rex

// A test over a real ssh, to a host of the user's: run it with
//
//	GOREX_E2E_DEST=me@box go test -tags sshe2e -run SSH -v ./internal/rex/
//
// GOREX_E2E_ARGS gives ssh more arguments (as "-F ./config"), and
// GOREX_E2E_COMMAND the GoRex there (as "env GOREX_DIR=/tmp/x ~/gorex",
// to keep off the sessions of the GoRex in use there).

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSSH(t *testing.T) {
	dest := os.Getenv("GOREX_E2E_DEST")
	if dest == "" {
		t.Skip("GOREX_E2E_DEST names no host")
	}
	r := Remote{Destination: dest, Command: os.Getenv("GOREX_E2E_COMMAND"), Args: strings.Fields(os.Getenv("GOREX_E2E_ARGS"))}
	start := time.Now()
	c := Dial(r.Connector(), DialOptions{Scrollback: 1 << 20, Backlog: 1 << 20})
	defer c.Close()
	st := waitState(t, c, Connected, 1)
	h, _ := c.Server()
	c.mu.Lock()
	args := c.tr.(*sshTransport).cmd.Args
	c.mu.Unlock()
	t.Logf("connected in %v to %s (%s, %s), ssh %s", time.Since(start), h.Host.Name, h.Host.OS, h.Platform, args[1])
	if args[1] != "-tt" {
		t.Errorf("the pseudo-terminal failed the probe: ssh %q", args)
	}

	start = time.Now()
	s := c.StreamCreate(CreateOptions{Command: []string{"/bin/sh"}}, 80, 24)
	defer s.Close()
	s.Resize(80, 24)
	s.Write([]byte("echo he''llo\n"))
	var out bytes.Buffer
	readUntil(t, s, &out, "hello")
	t.Logf("a session and its first output in %v", time.Since(start))

	// Keystrokes one at a time, as typed: each echo in about a round trip.
	time.Sleep(pingEvery + time.Second)
	st = c.Status()
	var worst, sum time.Duration
	const keys = 20
	for i := range keys {
		out.Reset()
		ch := string(rune('a' + i))
		at := time.Now()
		s.Write([]byte(ch))
		readUntil(t, s, &out, ch)
		d := time.Since(at)
		sum += d
		worst = max(worst, d)
		time.Sleep(30 * time.Millisecond) // faster than a round trip, as typing is
	}
	s.Write([]byte("\x15")) // ^U: the line typed goes
	t.Logf("rtt %v; echo of a key %v on average, %v at worst", st.RTT, sum/keys, worst)
	if st.RTT > 0 && worst > 3*st.RTT+50*time.Millisecond {
		t.Errorf("a keystroke took %v, for a round trip of %v", worst, st.RTT)
	}

	// Output in bulk comes through, and keys still get through it.
	out.Reset()
	s.Write([]byte("i=0; while [ $i -lt 20000 ]; do echo line $i; i=$((i+1)); done; echo fin''ished\n"))
	at := time.Now()
	readUntil(t, s, &out, "finished")
	t.Logf("20000 lines in %v", time.Since(at))

	// A flood of output, then ^C: the shell answers soon after, as the
	// output in flight is bounded, not all that the program printed.
	s.Write([]byte("yes flood-flood-flood-flood-flood-flood\n"))
	time.Sleep(3 * time.Second)
	at = time.Now()
	s.Write([]byte("\x03"))
	time.Sleep(50 * time.Millisecond)
	s.Write([]byte("echo calm''ed\n"))
	out.Reset()
	readUntil(t, s, &out, "calmed")
	t.Logf("^C to a shell answering after a flood: %v", time.Since(at))
	if d := time.Since(at); d > 10*time.Second {
		t.Errorf("the shell answered %v after ^C", d)
	}

	// The link drops and comes back, the session as it was.
	_, epoch := c.Server()
	at = time.Now()
	c.Reconnect()
	waitState(t, c, Connected, epoch+1)
	t.Logf("connected again in %v", time.Since(at))
	out.Reset()
	readUntil(t, s, &out, resetScreen)
	s.Write([]byte("echo ag''ain\n"))
	readUntil(t, s, &out, "again")
	t.Logf("attached again in %v", time.Since(at))
	at = time.Now()
	c.Kill(s.SID())
	t.Logf("killed in %v", time.Since(at))
}

// TestSSHInstall installs this test binary, as GoRex, into ~/.gorex/bin
// on the host, then connects to it there. GOREX_E2E_INSTALL must be the
// host's home directory, as $HOME says it there: the test checks it
// before writing anything, so as not to install where it was not meant.
func TestSSHInstall(t *testing.T) {
	dest, home := os.Getenv("GOREX_E2E_DEST"), os.Getenv("GOREX_E2E_INSTALL")
	if dest == "" || home == "" {
		t.Skip("GOREX_E2E_DEST and GOREX_E2E_INSTALL name no host to install on")
	}
	r := Remote{Destination: dest, Args: strings.Fields(os.Getenv("GOREX_E2E_ARGS"))}
	prog, args := r.sshArgs(false, `sh -c 'echo "$HOME"'`)
	out, err := exec.Command(prog, args...).Output()
	if got := strings.TrimSpace(string(out)); err != nil || got != home {
		t.Fatalf("the host's home is %q (%v), not %q: not installing", got, err, home)
	}
	var last int64
	start := time.Now()
	if err := r.Install(t.Context(), func(sent, total int64) { last = total }); err != nil {
		t.Fatal(err)
	}
	t.Logf("installed %d bytes in %v", last, time.Since(start))
	c := Dial(r.Connector(), DialOptions{})
	defer c.Close()
	waitState(t, c, Connected, 1)
	h, _ := c.Server()
	t.Logf("connected to the installed GoRex: %s, pid %d", h.Exe, h.PID)
	if !strings.Contains(h.Exe, installDir) {
		t.Errorf("the server runs %s", h.Exe)
	}
	c.Shutdown()
}
