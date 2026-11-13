//go:build darwin || linux

package rex

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary plays ssh, and the servers it starts: as ssh, it runs
// the bridge in the process, on a "remote" GoRex directory, behind a link
// of a given delay each way.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-server" {
		if err := Serve(); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-bridge" {
		// As a GoRex installed on a host.
		if err := Bridge(); err != nil {
			os.Exit(1)
		}
		return
	}
	if os.Getenv("GOREX_TEST_ROLE") == "ssh" {
		os.Exit(fakeSSH())
	}
	os.Exit(m.Run())
}

func fakeSSH() int {
	os.Setenv("GOREX_TEST_ROLE", "")
	os.Setenv("GOREX_DIR", os.Getenv("GOREX_TEST_REMOTE_DIR"))
	pty := false
	for _, a := range os.Args[1:] {
		pty = pty || a == "-tt"
	}
	if f := os.Getenv("GOREX_TEST_COUNT"); f != "" {
		if out, err := os.OpenFile(f, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			out.WriteString("ssh\n")
			out.Close()
		}
	}
	if os.Getenv("GOREX_TEST_FAIL") != "" {
		os.Stderr.WriteString("ssh: connect to host testhost port 22: Connection refused\r\n")
		return 255
	}
	// A profile's greeting, before anything.
	os.Stdout.WriteString("Welcome to the test host!\r\nLast login: never\n")
	if os.Getenv("GOREX_TEST_MISSING") != "" {
		os.Stdout.WriteString("\n" + missingBanner + "Linux x86_64\n")
		return 127
	}
	d, _ := time.ParseDuration(os.Getenv("GOREX_TEST_DELAY"))
	var out io.Writer = os.Stdout
	if pty && os.Getenv("GOREX_TEST_MANGLE") != "" {
		// A terminal that is not raw: newlines become CR LF.
		out = &crlf{w: out}
	}
	if err := bridge(delayReader(os.Stdin, d), delayWriter(out, d)); err != nil {
		return 1
	}
	return 0
}

type crlf struct{ w io.Writer }

func (c *crlf) Write(p []byte) (int, error) {
	_, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n")))
	return len(p), err
}

// delayReader delivers what r reads d later.
func delayReader(r io.ReadCloser, d time.Duration) io.ReadCloser {
	if d == 0 {
		return r
	}
	pr, pw := io.Pipe()
	type chunk struct {
		at time.Time
		b  []byte
	}
	ch := make(chan chunk, 4096)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				ch <- chunk{time.Now().Add(d), append([]byte(nil), buf[:n]...)}
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	go func() {
		for c := range ch {
			time.Sleep(time.Until(c.at))
			if _, err := pw.Write(c.b); err != nil {
				break
			}
		}
		pw.Close()
	}()
	return pr
}

// delayWriter writes to w d after.
func delayWriter(w io.Writer, d time.Duration) io.Writer {
	if d == 0 {
		return w
	}
	pr, pw := io.Pipe()
	r := delayReader(pr, d)
	go io.Copy(w, r)
	return pw
}

type remoteEnv struct {
	remoteDir string
	client    *Client
}

func newRemote(t *testing.T, delay time.Duration) *remoteEnv {
	return newRemoteWith(t, delay, DialOptions{Scrollback: 1 << 20, Backlog: 1 << 20})
}

func newRemoteWith(t *testing.T, delay time.Duration, o DialOptions) *remoteEnv {
	t.Helper()
	local, err := os.MkdirTemp("/tmp", "rexl")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := os.MkdirTemp("/tmp", "rexr")
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	t.Setenv("GOREX_DIR", local)
	t.Setenv("GOREX_TEST_ROLE", "ssh")
	t.Setenv("GOREX_TEST_REMOTE_DIR", remote)
	t.Setenv("GOREX_TEST_DELAY", delay.String())
	e := &remoteEnv{remoteDir: remote}
	e.client = Dial(Remote{SSH: exe, Destination: "testhost"}.Connector(), o)
	t.Cleanup(func() {
		// End the remote server, with its sessions.
		if h, _ := e.client.Server(); h.PID > 0 {
			syscall.Kill(h.PID, syscall.SIGTERM)
			e.client.Shutdown()
		}
		e.client.Close()
		time.Sleep(100 * time.Millisecond)
		os.RemoveAll(local)
		os.RemoveAll(remote)
	})
	return e
}

func waitState(t *testing.T, c *Client, want State, epoch int) Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := c.Status()
		_, e := c.Server()
		if st.State == want && e >= epoch {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %v (epoch %d, err %v), waiting for %v (epoch %d)", st.State, e, st.Err, want, epoch)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readUntil reads s until what it read has want, after mark.
func readUntil(t *testing.T, s *Stream, out *bytes.Buffer, want string) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		for !strings.Contains(out.String(), want) {
			n, err := s.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				got <- err
				return
			}
		}
		got <- nil
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("reading for %q: %v; read %q", want, err, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no %q; read %q", want, out.String())
	}
}

func TestRemote(t *testing.T) {
	e := newRemote(t, 60*time.Millisecond)
	c := e.client
	waitState(t, c, Connected, 1)
	h, _ := c.Server()
	if h.Platform == "" || h.Host.Name == "" {
		t.Errorf("hello %+v", h)
	}

	// A session starts as the stream attaches, in the remote directory.
	start := time.Now()
	s := c.StreamCreate(CreateOptions{Command: []string{"/bin/sh"}, Dir: "/tmp"}, 80, 24)
	s.Resize(80, 24)
	s.Write([]byte("echo he''llo\n"))
	var out bytes.Buffer
	readUntil(t, s, &out, "hello")
	t.Logf("first output after %v", time.Since(start))
	if st := c.Status(); st.RTT < 100*time.Millisecond {
		// The link has 60 ms each way: the pings measure it.
		time.Sleep(pingEvery + time.Second)
		if st := c.Status(); st.RTT < 100*time.Millisecond {
			t.Errorf("rtt %v", st.RTT)
		}
	}
	list, err := c.List()
	if err != nil || len(list) != 1 || list[0].ID != s.SID() {
		t.Fatalf("list %+v %v", list, err)
	}

	// The link drops: the client connects again, and the stream attaches
	// again, to the same session, its screen reset and drawn anew.
	c.mu.Lock()
	tr := c.tr.(*sshTransport)
	c.mu.Unlock()
	tr.kill()
	waitState(t, c, Connected, 2)
	out.Reset()
	readUntil(t, s, &out, resetScreen)
	readUntil(t, s, &out, "hello") // the snapshot
	s.Write([]byte("echo ag''ain\n"))
	readUntil(t, s, &out, "again")
	if list, _ := c.List(); len(list) != 1 {
		t.Errorf("sessions after reconnecting %+v", list)
	}

	// The remote server dies, as when its machine restarts: the session
	// starts again, of the same ID, on the server started in its place.
	h1, _ := c.Server()
	syscall.Kill(h1.PID, syscall.SIGKILL)
	waitState(t, c, Connected, 3)
	if h2, _ := c.Server(); h2.Instance() == h1.Instance() {
		t.Fatal("the same server")
	}
	out.Reset()
	readUntil(t, s, &out, resetScreen)
	s.Write([]byte("echo re''born\n"))
	readUntil(t, s, &out, "reborn")
	if list, _ := c.List(); len(list) != 1 || list[0].ID != s.SID() {
		t.Errorf("sessions on the new server %+v", list)
	}

	// Kill while the link is down: it is sent once the client is back.
	c.mu.Lock()
	tr = c.tr.(*sshTransport)
	c.mu.Unlock()
	tr.kill()
	for ok, _ := c.connected(); ok; ok, _ = c.connected() {
		time.Sleep(5 * time.Millisecond)
	}
	s.Close()
	if err := c.Kill(s.SID()); err != nil {
		t.Fatal(err)
	}
	_, epoch := c.Server()
	waitState(t, c, Connected, epoch+1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := c.List()
		if err == nil && len(list) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions left %+v %v", list, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A session that exits ends its stream.
	s = c.StreamCreate(CreateOptions{Command: []string{"/bin/sh"}}, 80, 24)
	s.Resize(80, 24)
	s.Write([]byte("exit\n"))
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, s)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream of a session that exited goes on")
	}
}

func TestRemoteMissing(t *testing.T) {
	t.Setenv("GOREX_TEST_MISSING", "1")
	e := newRemote(t, 0)
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := e.client.Status()
		var m *MissingError
		if errors.As(st.Err, &m) {
			if m.Platform() != "linux/amd64" {
				t.Errorf("platform %q", m.Platform())
			}
			if st.State == Waiting && st.RetryAt.Sub(time.Now()) < 30*time.Second {
				t.Errorf("retries in %v", time.Until(st.RetryAt))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A pseudo-terminal that changes bytes fails the probe: the client runs
// the bridge without one.
func TestRemoteMangled(t *testing.T) {
	t.Setenv("GOREX_TEST_MANGLE", "1")
	e := newRemote(t, 0)
	waitState(t, e.client, Connected, 1)
	s := e.client.StreamCreate(CreateOptions{Command: []string{"/bin/sh"}}, 80, 24)
	s.Resize(80, 24)
	s.Write([]byte("echo fi''ne\n"))
	var out bytes.Buffer
	readUntil(t, s, &out, "fine")
	e.client.mu.Lock()
	args := e.client.tr.(*sshTransport).cmd.Args
	e.client.mu.Unlock()
	if args[1] != "-T" {
		t.Errorf("ssh %q", args)
	}
}

func TestRemoteValid(t *testing.T) {
	for _, d := range []string{"", "-oProxyCommand=x", "a b", "h;rm"} {
		if (Remote{Destination: d}).Valid() == nil {
			t.Errorf("%q is valid", d)
		}
	}
	for _, d := range []string{"box", "me@box.local", "me@10.0.0.2", "alias-1"} {
		if err := (Remote{Destination: d}).Valid(); err != nil {
			t.Errorf("%q: %v", d, err)
		}
	}
}

func TestCoalescer(t *testing.T) {
	var w bytes.Buffer
	c := &coalescer{w: &w}
	hdr := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 3}
	c.Write(hdr)
	if w.Len() != 0 {
		t.Fatal("wrote the header alone")
	}
	c.Write([]byte("abc"))
	if !bytes.Equal(w.Bytes(), append(hdr, "abc"...)) {
		t.Fatalf("wrote %q", w.Bytes())
	}
	w.Reset()
	ping := []byte{0, 2, 0, 1, 0, 0, 0, 0, 0, 0, 0, 7}
	c.Write(ping)
	if !bytes.Equal(w.Bytes(), ping) {
		t.Fatalf("held a ping back")
	}
}

// A host out of reach is tried MaxRetries times, then left until kicked,
// which tries it as many times again.
func TestRemoteMaxRetries(t *testing.T) {
	count := t.TempDir() + "/count"
	t.Setenv("GOREX_TEST_COUNT", count)
	t.Setenv("GOREX_TEST_FAIL", "1")
	e := newRemoteWith(t, 0, DialOptions{MaxRetries: 3})
	c := e.client
	st := waitState(t, c, GaveUp, 0)
	if st.Attempts != 3 || st.Err == nil || !strings.Contains(st.Err.Error(), "Connection refused") {
		t.Errorf("gave up %+v", st)
	}
	attempts := func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "\n")
	}
	time.Sleep(2 * time.Second)
	if n := attempts(); n != 3 {
		t.Errorf("%d attempts, not 3", n)
	}
	// Kicked, it tries again, and connects to a host back.
	os.Setenv("GOREX_TEST_FAIL", "")
	c.Kick()
	waitState(t, c, Connected, 1)

	// Losing a working link gives it a fresh count.
	os.Setenv("GOREX_TEST_FAIL", "1")
	c.Reconnect()
	st = waitState(t, c, GaveUp, 1)
	if st.Attempts != 3 {
		t.Errorf("gave up after %d attempts, after losing the link", st.Attempts)
	}
	if n := attempts(); n != 7 {
		t.Errorf("%d attempts in all, not 7", n)
	}
}
