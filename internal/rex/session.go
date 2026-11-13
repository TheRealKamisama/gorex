//go:build darwin || linux || windows

package rex

import (
	"bytes"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
)

// scrollback is about how many bytes of output a session keeps above its
// screen, for the windows that attach to it.
const scrollback = 8 << 20

// session is a pseudo-terminal of the server and its shell.
type session struct {
	id      string
	shell   string
	created time.Time
	p       *pty

	mu sync.Mutex
	// vt is the session's screen, kept by the terminal emulator of the
	// app's terminals without a view: a window attaching gets a snapshot
	// of it at its size, as the session shows it now.
	vt         *terminal.Terminal
	vtIn       *io.PipeWriter
	output     uint64
	bells      atomic.Int64
	clients    map[*attached]struct{}
	lastOutput time.Time
	lastInput  time.Time
	resync     *time.Timer
	exited     bool
	code       int
	cols, rows int
	done       chan struct{}
}

// attached is a connection attached to a session, whose output a
// goroutine writes so that a slow client never holds up the others.
type attached struct {
	conn net.Conn
	out  chan []byte
	once sync.Once
	// first is the snapshot the writer sends before the output, made to
	// the client's size of scrollback, aside from the session's lock.
	first      []byte
	cols, rows int
	scrollback int
	// queued is how many bytes of output wait for the writer, and backlog
	// how many may before the client is dropped.
	queued  atomic.Int64
	backlog int64
	// dropped tells a client dropped as too far behind, whose writer
	// drops what is left to write: attaching again, it gets the screen.
	dropped atomic.Bool
}

// defaultBacklog is the bytes of output that may wait for a client that
// gives no backlog of its own: a window on this machine, which only falls
// that far behind when it hangs.
const defaultBacklog = 64 << 20

func (a *attached) send(p []byte) bool {
	if a.queued.Add(int64(len(p))) > a.backlog {
		return false // too far behind: drop it, it attaches again
	}
	select {
	case a.out <- p:
		return true
	default:
		return false
	}
}

// finish ends the output once the writer wrote what waits.
func (a *attached) finish() { a.once.Do(func() { close(a.out) }) }

// drop ends the output at once.
func (a *attached) drop() {
	a.dropped.Store(true)
	a.finish()
}

func (a *attached) writer() {
	if a.scrollback > 0 && len(a.first) > a.scrollback {
		a.first = trimSnapshot(a.first, a.cols, a.rows, a.scrollback)
	}
	_, err := a.conn.Write(a.first)
	a.first = nil
	for p := range a.out {
		a.queued.Add(-int64(len(p)))
		if err != nil || a.dropped.Load() {
			continue
		}
		_, err = a.conn.Write(p)
	}
	a.conn.Close()
}

// trimSnapshot returns a snapshot with about limit bytes of scrollback: a
// terminal of so much scrollback is fed it, and snapshot in turn.
func trimSnapshot(snap []byte, cols, rows, limit int) []byte {
	pr, pw := io.Pipe()
	defer pw.Close()
	vt, err := terminal.New(terminal.Options{Conn: discard{pr}, Scrollback: limit})
	if err != nil {
		return snap
	}
	defer vt.Close()
	vt.Resize(cols, rows)
	vt.Feed(snap)
	if out := vt.Snapshot(); len(out) > 0 {
		return out
	}
	return snap
}

func newSession(id string, o CreateOptions) (*session, error) {
	path, argv, name, err := shellCommand(o.Command)
	if err != nil {
		return nil, err
	}
	cols, rows := o.Cols, o.Rows
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	dir := o.Dir
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		// As when the directory of a pane a client restores is gone.
		dir, _ = os.UserHomeDir()
	}
	env := sessionEnv(id, o.Env)
	p, err := startPTY(path, argv, dir, env, cols, rows)
	if err != nil {
		return nil, err
	}
	s := &session{
		id: id, shell: name, created: time.Now(), p: p,
		clients: map[*attached]struct{}{},
		cols:    cols, rows: rows, done: make(chan struct{}),
	}
	// The emulator's Conn reads nothing, and what it would answer programs
	// is dropped: the windows' terminals answer them.
	pr, pw := io.Pipe()
	vt, err := terminal.New(terminal.Options{Conn: discard{pr}, Scrollback: scrollback, OnBell: func() { s.bells.Add(1) }})
	if err != nil {
		p.hangup()
		return nil, err
	}
	vt.Resize(cols, rows)
	s.vt, s.vtIn = vt, pw
	read := make(chan struct{})
	go func() {
		s.read()
		close(read)
	}()
	go func() {
		code := p.wait()
		// What it printed last is still to read; then hang up whatever
		// else holds the terminal, as terminal apps do.
		select {
		case <-read:
		case <-time.After(150 * time.Millisecond):
			p.close()
			<-read
		}
		s.mu.Lock()
		s.exited, s.code = true, code
		for a := range s.clients {
			a.finish()
		}
		clear(s.clients)
		s.mu.Unlock()
		close(s.done)
		s.vtIn.Close()
		s.vt.Close()
	}()
	return s, nil
}

// sessionEnv returns the environment of a session's shell: the server's,
// without what other terminals set, and what terminal apps set.
func sessionEnv(id string, extra []string) []string {
	var env []string
	hasLang := false
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "TERM", k == "COLORTERM", k == "TERM_PROGRAM", k == "TERM_PROGRAM_VERSION",
			k == "TERM_SESSION_ID", k == "TMUX", k == "TMUX_PANE", k == "STY", k == "WINDOWID",
			k == "COLUMNS", k == "LINES", k == "SHLVL", k == "OLDPWD", k == "PWD", k == "_",
			k == "SSH_TTY", // the bridge's, when ssh started the server
			strings.HasPrefix(k, "ITERM_"), strings.HasPrefix(k, "GHOSTTY_"), strings.HasPrefix(k, "KITTY_"),
			strings.HasPrefix(k, "VSCODE_"), strings.HasPrefix(k, "WEZTERM_"), strings.HasPrefix(k, "ALACRITTY_"),
			strings.HasPrefix(k, "GOREX_"), strings.HasPrefix(k, "MYGO_"):
			continue
		case k == "LANG" || k == "LC_ALL" || k == "LC_CTYPE":
			hasLang = true
		}
		env = append(env, kv)
	}
	env = append(env,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"TERM_PROGRAM=GoRex",
		"TERM_PROGRAM_VERSION=0.1.0",
		"GOREX_SESSION="+id,
	)
	if !hasLang {
		env = append(env, "LANG=en_US.UTF-8")
	}
	return append(env, extra...)
}

func (s *session) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.p.read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.vt.Feed(data)
			s.output += uint64(n)
			s.lastOutput = time.Now()
			for a := range s.clients {
				if !a.send(data) {
					a.drop()
					delete(s.clients, a)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// attach attaches a connection: it gets a snapshot of the session's
// screen and scrollback at its size, then what the session prints, and
// what it sends is typed.
func (s *session) attach(conn net.Conn, at Attach) {
	cols, rows := at.Cols, at.Rows
	a := &attached{conn: conn, out: make(chan []byte, 2048), scrollback: at.Scrollback, backlog: int64(at.Backlog)}
	if a.backlog <= 0 {
		a.backlog = defaultBacklog
	}
	s.mu.Lock()
	if cols > 0 && rows > 0 {
		// The screen reflows to the window's size, which the program
		// learns next.
		s.vt.Resize(cols, rows)
	}
	a.cols, a.rows = s.cols, s.rows
	if cols > 0 && rows > 0 {
		a.cols, a.rows = cols, rows
	}
	snap := s.vt.Snapshot()
	if title := s.vt.Title(); title != "" {
		snap = append([]byte("\x1b]2;"+title+"\x07"), snap...)
	}
	a.first = snap
	exited := s.exited
	if exited {
		a.finish()
	} else {
		s.clients[a] = struct{}{}
	}
	s.mu.Unlock()
	go a.writer()
	if exited {
		return
	}
	if cols > 0 && rows > 0 {
		s.resize(cols, rows)
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			s.input(buf[:n])
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	if _, ok := s.clients[a]; ok {
		delete(s.clients, a)
		a.finish()
	}
	s.mu.Unlock()
}

func (s *session) input(p []byte) {
	s.mu.Lock()
	s.lastInput = time.Now()
	exited := s.exited
	s.mu.Unlock()
	if !exited {
		s.p.write(p)
	}
}

// resize sets the size of the terminal and reports whether it changed.
func (s *session) resize(cols, rows int) bool {
	s.mu.Lock()
	changed := cols != s.cols || rows != s.rows
	s.cols, s.rows = cols, rows
	exited := s.exited
	if changed {
		s.vt.Resize(cols, rows)
		// Once the program drew its screen at the new size, the windows
		// get it again.
		if s.resync == nil {
			s.resync = time.AfterFunc(resyncDelay, s.resyncScreen)
		} else {
			s.resync.Reset(resyncDelay)
		}
	}
	s.mu.Unlock()
	if changed && !exited {
		s.p.resize(cols, rows)
	}
	return changed
}

// resyncDelay is how long after the last resize the screen of a
// full-screen program is sent again.
const resyncDelay = 150 * time.Millisecond

// resyncScreen sends the windows the screen of a full-screen program as
// the session has it. A window resizes its terminal as it draws, and the
// session as the request reaches it, while the program's output is on its
// way: what the program drew for one size may land in a screen of
// another, which full-screen programs, redrawing only what changed, do
// not mend. Shells' scrollback reflows alike in both and needs nothing.
func (s *session) resyncScreen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || len(s.clients) == 0 {
		return
	}
	snap := s.vt.Snapshot()
	if !bytes.Contains(snap, []byte("\x1b[?1049h")) {
		return
	}
	// Clear the alternate screen the window shows, then draw it anew.
	msg := append([]byte("\x1b[?1049h\x1b[H\x1b[2J"), snap...)
	for a := range s.clients {
		if !a.send(msg) {
			a.drop()
			delete(s.clients, a)
		}
	}
}

// kill hangs up the session and waits a little for it to end.
func (s *session) kill() {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if !exited {
		s.p.hangup()
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		s.p.kill()
	}
}

// info describes the session and what runs in its foreground.
func (s *session) info() SessionInfo {
	s.mu.Lock()
	in := SessionInfo{
		ID: s.id, Shell: s.shell, Created: s.created,
		Title: s.vt.Title(), LastOutput: s.lastOutput, LastInput: s.lastInput,
		Output: s.output, Bells: int(s.bells.Load()), Exited: s.exited, ExitCode: s.code,
		Attached: len(s.clients), Cols: s.cols, Rows: s.rows,
	}
	s.mu.Unlock()
	in.PID = s.p.pid()
	if in.Exited {
		return in
	}
	fg := s.p.foreground()
	if fg <= 0 {
		fg = in.PID
	}
	in.Idle = fg == in.PID
	p := inspect(fg)
	in.Program, in.Args, in.Dir = p.programName(), p.args, p.dir
	if in.Program == "" {
		in.Program = s.shell
	}
	if in.Dir == "" && !in.Idle {
		in.Dir = inspect(in.PID).dir
	}
	return in
}

// discard is the Conn of a session's emulator: it reads from a pipe that
// closes with the session, and drops what it writes.
type discard struct{ r *io.PipeReader }

func (d discard) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d discard) Write(p []byte) (int, error) { return len(p), nil }
func (d discard) Close() error                { return d.r.Close() }
