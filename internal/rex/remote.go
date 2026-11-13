package rex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/hashicorp/yamux"
)

// Remote is a machine whose session server a client reaches through ssh:
// ssh runs GoRex there as a bridge (gorex -bridge), which carries the
// client's connections to that machine's server, starting one when none
// runs. Its sessions live there: losing the link loses nothing, and the
// client attaches to them again once it connects again.
//
// ssh is the system's, so that ~/.ssh/config, keys, the agent, ProxyJump
// and known hosts work as in a terminal; it runs without asking anything
// (BatchMode), as a window has no terminal to ask in: a host must let the
// user in with a key or an agent.
type Remote struct {
	// Destination is ssh's: [user@]host, or a Host of ~/.ssh/config.
	Destination string
	// Command runs GoRex on the host, as "~/bin/gorex": empty, the bridge
	// is looked for where GoRex installs it and on the PATH.
	Command string
	// SSH is the ssh program, "ssh" when empty, and Args go before the
	// destination, as "-p 2222" or "-J jump".
	SSH  string
	Args []string

	// plain remembers that the host's pseudo-terminal does not carry
	// bytes as they are (as Windows's), and to run the bridge without one.
	plain *atomic.Bool
}

// Valid reports what is wrong with the remote, if anything.
func (r Remote) Valid() error {
	d := r.Destination
	switch {
	case strings.TrimSpace(d) == "":
		return errors.New("no host given")
	case strings.HasPrefix(d, "-"):
		return errors.New("a host does not start with -")
	case strings.ContainsAny(d, " \t\r\n'\"`$;&|<>\\"):
		return errors.New("a host has no spaces or quotes")
	}
	return nil
}

// MissingError tells that GoRex is not on the host, of which OS and Arch
// (as uname says them).
type MissingError struct {
	OS, Arch string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("GoRex is not installed on the host (%s %s)", e.OS, e.Arch)
}

// Platform returns the host's GOOS/GOARCH, or "" when Go has no name for
// it.
func (e *MissingError) Platform() string {
	goos := map[string]string{"Linux": "linux", "Darwin": "darwin", "FreeBSD": "freebsd"}[e.OS]
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[e.Arch]
	if goos == "" || arch == "" {
		return ""
	}
	return goos + "/" + arch
}

// CanInstall tells that this machine's GoRex runs on the host: it is of
// the same system and processor.
func (e *MissingError) CanInstall() bool { return e.Platform() == runtime.GOOS+"/"+runtime.GOARCH }

// The bridge's first line says it speaks the protocol; a host without
// GoRex says what it is, for the client to offer installing it.
const (
	bridgeBanner  = "GOREX-BRIDGE "
	missingBanner = "GOREX-MISSING "
	errorBanner   = "GOREX-ERROR "
	// probeSize is the bytes of every value the client sends and the
	// bridge echoes, which checks that the pipe carries bytes as they are.
	probeSize = 256
)

// installDir is where Install puts GoRex on a host, from its home.
const installDir = ".gorex/bin"

// remoteScript runs the bridge: command, or the first GoRex found, or
// says the host has none. The login shell of the host runs it with sh,
// whatever it is (fish too): it has no single quote or backslash.
func remoteScript(command string) string {
	if command != "" {
		return command + " -bridge"
	}
	return `sh -c 'for g in "$GOREX_BRIDGE" "$HOME/` + installDir + `/gorex" "$HOME/.local/bin/gorex" "$HOME/go/bin/gorex" /usr/local/bin/gorex gorex; do ` +
		`if [ -n "$g" ] && command -v "$g" >/dev/null 2>&1; then exec "$g" -bridge; fi; done; ` +
		`echo; echo "` + missingBanner + `$(uname -s) $(uname -m)"; exit 127'`
}

// Connector returns the connector of a client of the remote.
func (r Remote) Connector() Connector {
	if r.plain == nil {
		r.plain = new(atomic.Bool)
	}
	return func(ctx context.Context) (Transport, error) {
		if err := r.Valid(); err != nil {
			return nil, err
		}
		if !r.plain.Load() {
			t, err := r.dial(ctx, true)
			if !errors.Is(err, errProbe) {
				return t, err
			}
			r.plain.Store(true)
		}
		return r.dial(ctx, false)
	}
}

// errProbe is the error of a pipe that did not carry the probe as it is.
var errProbe = errors.New("rex: the link does not carry bytes as they are")

// sshArgs returns the arguments of ssh. With a pseudo-terminal (-tt), ssh
// sends each keystroke at once (TCP_NODELAY), where it holds small writes
// back without one; the bridge makes the terminal raw before saying a
// word. Escapes are off, as the stream is not typed, and keepalives find
// a dead link within half a minute.
func (r Remote) sshArgs(pty bool, script string) (string, []string) {
	prog := r.SSH
	if prog == "" {
		prog = "ssh"
	}
	args := []string{"-T"}
	if pty {
		args = []string{"-tt"}
	}
	args = append(args, "-e", "none",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=20",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
	)
	args = append(args, r.Args...)
	args = append(args, r.Destination, script)
	return prog, args
}

// dial runs ssh and the bridge, and checks the pipe between them.
func (r Remote) dial(ctx context.Context, pty bool) (*sshTransport, error) {
	prog, args := r.sshArgs(pty, remoteScript(r.Command))
	cmd := exec.Command(prog, args...)
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tail{max: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("running ssh: %w", err)
	}
	t := &sshTransport{cmd: cmd, stdin: stdin, stderr: stderr, exited: make(chan struct{})}
	go func() {
		t.waitErr = cmd.Wait()
		close(t.exited)
	}()
	br := bufio.NewReaderSize(stdout, 64<<10)
	type result struct{ err error }
	done := make(chan result, 1)
	go func() { done <- result{t.handshake(br, pty)} }()
	select {
	case res := <-done:
		err = res.err
	case <-ctx.Done():
		err = fmt.Errorf("timed out reaching %s", r.Destination)
	}
	if err != nil {
		t.kill()
		<-t.exited
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = t.sshError()
		}
		return nil, err
	}
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false // the client pings, and measures
	cfg.ConnectionWriteTimeout = 20 * time.Second
	cfg.StreamOpenTimeout = 30 * time.Second
	cfg.StreamCloseTimeout = 30 * time.Second
	cfg.LogOutput = io.Discard
	// Closing the link ends ssh: over a pseudo-terminal, the end of its
	// input reaches no one, and yamux waits for its output to end.
	sess, err := yamux.Client(&pipe{r: br, w: &coalescer{w: stdin}, c: closer(t.end)}, cfg)
	if err != nil {
		t.kill()
		return nil, err
	}
	t.sess = sess
	go func() {
		select {
		case <-t.exited:
			sess.Close()
		case <-sess.CloseChan():
		}
	}()
	return t, nil
}

// handshake skips what the host prints before the bridge (a profile's
// greeting), reads its banner, and has it echo the probe.
func (t *sshTransport) handshake(br *bufio.Reader, pty bool) error {
	skipped := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		skipped += len(line)
		line = strings.TrimRight(line, "\r\n")
		// A pseudo-terminal may have echoed nothing, but put \r around.
		line = strings.TrimLeft(line, "\r")
		switch {
		case strings.HasPrefix(line, bridgeBanner):
			f := strings.Fields(strings.TrimPrefix(line, bridgeBanner))
			if len(f) > 0 && f[0] != fmt.Sprint(ProtocolVersion) {
				platform := ""
				if len(f) > 1 {
					platform = f[1]
				}
				var v int
				fmt.Sscan(f[0], &v)
				return &VersionError{Version: v, Platform: platform}
			}
			probe := make([]byte, probeSize)
			for i := range probe {
				probe[i] = byte(i)
			}
			if _, err := t.stdin.Write(probe); err != nil {
				return err
			}
			// A pseudo-terminal that swallows bytes never echoes them all.
			var late atomic.Bool
			timer := time.AfterFunc(15*time.Second, func() {
				late.Store(true)
				t.kill()
			})
			echo := make([]byte, probeSize)
			_, err := io.ReadFull(br, echo)
			timer.Stop()
			if err != nil {
				if pty && late.Load() {
					return errProbe
				}
				return err
			}
			if !bytes.Equal(echo, probe) {
				return errProbe
			}
			return nil
		case strings.HasPrefix(line, missingBanner):
			f := strings.Fields(strings.TrimPrefix(line, missingBanner))
			e := &MissingError{}
			if len(f) > 0 {
				e.OS = f[0]
			}
			if len(f) > 1 {
				e.Arch = f[1]
			}
			return e
		case strings.HasPrefix(line, errorBanner):
			return errors.New("GoRex on the host: " + strings.TrimPrefix(line, errorBanner))
		}
		if skipped > 1<<20 {
			return errors.New("the host printed too much before GoRex answered")
		}
	}
}

// sshTransport is ssh, the bridge it runs, and the connections between
// them, many on one pipe.
type sshTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *tail
	sess    *yamux.Session
	exited  chan struct{}
	waitErr error
	once    sync.Once
}

func (t *sshTransport) Dial() (net.Conn, error) { return t.sess.Open() }
func (t *sshTransport) Done() <-chan struct{}   { return t.sess.CloseChan() }

func (t *sshTransport) Ping() (time.Duration, error) { return t.sess.Ping() }

func (t *sshTransport) Close() error {
	t.end()
	if t.sess != nil {
		go t.sess.Close() // returns once ssh's output ends
	}
	return nil
}

// end ends ssh, once.
func (t *sshTransport) end() error {
	t.once.Do(func() {
		t.stdin.Close()
		t.kill()
	})
	return nil
}

type closer func() error

func (c closer) Close() error { return c() }

func (t *sshTransport) kill() {
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
}

// Reason tells why the link ended, as ssh said it, once it did.
func (t *sshTransport) Reason() error {
	select {
	case <-t.exited:
	case <-time.After(300 * time.Millisecond):
		return nil
	}
	if msg := t.stderr.lastLine(); msg != "" {
		return fmt.Errorf("the connection was lost: %s", msg)
	}
	return nil
}

// sshError returns why ssh ended: what it said last, or how it exited.
func (t *sshTransport) sshError() error {
	if msg := t.stderr.lastLine(); msg != "" {
		return errors.New(msg)
	}
	if t.waitErr != nil {
		return fmt.Errorf("ssh: %v", t.waitErr)
	}
	return errors.New("ssh ended before GoRex answered")
}

// pipe is ssh's standard output and input, as one connection.
type pipe struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

func (p *pipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipe) Close() error                { return p.c.Close() }

// coalescer writes yamux's header of a frame with the frame's body, in
// one write rather than two: one packet of ssh's, not two, per keystroke.
type coalescer struct {
	w   io.Writer
	hdr []byte
}

// yamux's header: version, type, flags, stream, length; type 0 is data.
const yamuxHeader = 12

func (c *coalescer) Write(p []byte) (int, error) {
	if c.hdr == nil && len(p) == yamuxHeader && p[1] == 0 && (p[8]|p[9]|p[10]|p[11]) != 0 {
		c.hdr = append(make([]byte, 0, 64<<10), p...)
		return len(p), nil
	}
	if c.hdr != nil {
		b := append(c.hdr, p...)
		c.hdr = nil
		if _, err := c.w.Write(b); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return c.w.Write(p)
}

// tail keeps the last bytes written to it: ssh's errors.
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(t.b), "\r", "")), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "Warning: Permanently added") {
			return l
		}
	}
	return ""
}

// Install copies this GoRex, and the terminal library its server needs,
// to the host, into ~/.gorex/bin, where the bridge is looked for first.
// The host must run the same system on the same processor; progress is
// told how many bytes went of how many.
func (r Remote) Install(ctx context.Context, progress func(sent, total int64)) error {
	if err := r.Valid(); err != nil {
		return err
	}
	exe, _ := Executable()
	if exe == "" {
		return errors.New("cannot find GoRex's executable")
	}
	lib, err := terminal.LibraryPath()
	if err != nil {
		return fmt.Errorf("cannot find the terminal library: %w", err)
	}
	files := []struct{ src, name string }{{lib, filepath.Base(lib)}, {exe, "gorex"}}
	var total, sent int64
	for _, f := range files {
		st, err := os.Stat(f.src)
		if err != nil {
			return err
		}
		total += st.Size()
	}
	for _, f := range files {
		if strings.ContainsAny(f.name, " '\"$/\\") {
			return fmt.Errorf("odd file name %q", f.name)
		}
		in, err := os.Open(f.src)
		if err != nil {
			return err
		}
		script := `sh -c 'umask 022; d="$HOME/` + installDir + `"; mkdir -p "$d" && cat > "$d/` + f.name + `.part" && chmod 755 "$d/` + f.name + `.part" && mv -f "$d/` + f.name + `.part" "$d/` + f.name + `"'`
		prog, args := r.sshArgs(false, script)
		cmd := exec.CommandContext(ctx, prog, args...)
		hideWindow(cmd)
		base := sent
		cmd.Stdin = &countingReader{r: in, n: func(n int64) {
			if progress != nil {
				progress(base+n, total)
			}
		}}
		stderr := &tail{max: 4096}
		cmd.Stderr = stderr
		err = cmd.Run()
		in.Close()
		if err != nil {
			if msg := stderr.lastLine(); msg != "" {
				return errors.New(msg)
			}
			return err
		}
		st, _ := os.Stat(f.src)
		sent += st.Size()
	}
	return nil
}

type countingReader struct {
	r   io.Reader
	sum int64
	n   func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.sum += int64(n)
	c.n(c.sum)
	return n, err
}
