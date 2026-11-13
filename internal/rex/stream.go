package rex

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Stream is a session attached to, for a terminal's Conn: it attaches
// once it knows the size of the screen, from the first Resize, or after a
// moment at the size it had last. It attaches again when its connection
// is lost while the session goes on, as when the link to another machine
// drops or the server dropped a client too far behind: the terminal then
// reads a reset and the screen as it is, and what the session printed
// meanwhile scrolls back above it.
type Stream struct {
	c   *Client
	sid string

	// wmu orders what is written: what was typed while attaching goes
	// before what is typed next.
	wmu  sync.Mutex
	mu   sync.Mutex
	cond *sync.Cond
	conn net.Conn
	// gen counts attachments, and reset tells that the next read starts
	// with resetting the terminal, for the screen of an attachment after
	// the first.
	gen   int
	reset bool
	// create starts the session when the server has none of it: at first,
	// and on a server that restarted (and lost its sessions).
	create *CreateOptions
	// server is the instance of the server attached to last.
	server string
	// pending is what was typed while attaching.
	pending []byte
	// end is how the stream ended, and failure what to show when it ended
	// as the session could not start.
	end       error
	failure   string
	failShown bool
	closed    bool
	done      chan struct{}
	lost      chan struct{}
	sized     chan struct{}
	resized   bool
	// cols and rows are the terminal's size; sentCols and sentRows the
	// size the server was told last.
	cols, rows         int
	sentCols, sentRows int

	// OnData, when set, is called with the size of what the session
	// prints, from the reading goroutine.
	OnData func(n int)
}

// Stream returns a stream of the session sid, which attaches lazily.
func (c *Client) Stream(sid string, cols, rows int) *Stream {
	return c.newStream(sid, nil, cols, rows)
}

// StreamCreate returns a stream that starts the session of o when the
// server has none of it, of ID o.ID or a new one. The session starts as
// the stream attaches, in the same round trip, which matters on a slow
// link; and starts again, in its last directory, after the server
// restarted.
func (c *Client) StreamCreate(o CreateOptions, cols, rows int) *Stream {
	if o.ID == "" {
		o.ID = newID()
	}
	return c.newStream(o.ID, &o, cols, rows)
}

func (c *Client) newStream(sid string, create *CreateOptions, cols, rows int) *Stream {
	s := &Stream{
		c: c, sid: sid, create: create, cols: cols, rows: rows,
		done: make(chan struct{}), lost: make(chan struct{}, 1), sized: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.run()
	return s
}

// SID returns the ID of the stream's session.
func (s *Stream) SID() string { return s.sid }

// SetDir sets the directory the session starts in again, should the
// server lose it.
func (s *Stream) SetDir(dir string) {
	s.mu.Lock()
	if s.create != nil && dir != "" {
		s.create.Dir = dir
	}
	s.mu.Unlock()
}

// Attached reports whether the stream is attached now.
func (s *Stream) Attached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// Failed reports whether the stream ended as its session could not
// start, which it showed.
func (s *Stream) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure != ""
}

// resetScreen resets the terminal for the screen of an attachment after
// the first: RIS, then clear the scrollback, which the snapshot brings.
const resetScreen = "\x1bc\x1b[3J"

// run attaches the stream, and attaches it again, until it closes or its
// session ends.
func (s *Stream) run() {
	select {
	case <-s.sized:
	case <-time.After(400 * time.Millisecond):
	case <-s.done:
		return
	}
	first := true
	var since time.Time
	for {
		conn, epoch, err := s.attach()
		if s.isClosed() {
			if conn != nil {
				conn.Close()
			}
			return
		}
		var serverErr *attachError
		switch {
		case errors.Is(err, ErrNoSession):
			s.finish(io.EOF, "")
			return
		case errors.As(err, &serverErr):
			s.finish(io.EOF, serverErr.msg)
			return
		case err != nil:
			select {
			case <-s.c.Closed():
				// No connection will come.
				if ok, _ := s.c.connected(); !ok {
					s.finish(io.EOF, "")
					return
				}
			default:
			}
			if ok, e := s.c.connected(); ok && e == epoch {
				// Connected, yet attaching failed: not too fast.
				if !s.sleep(time.Second) {
					return
				}
			} else {
				s.c.waitChange(s.done)
			}
			continue
		}
		s.publish(conn, !first)
		first = false
		since = time.Now()
		s.sync()
		select {
		case <-s.lost:
		case <-s.done:
			conn.Close()
			return
		}
		conn.Close()
		// The connection ended: the session may have.
		if ok, e := s.c.connected(); ok && e == epoch {
			in, err := s.c.Info(s.sid)
			if err == nil && in.Exited || errors.Is(err, ErrNoSession) || err != nil && strings.HasPrefix(err.Error(), "unknown op") {
				// Exited, gone, or a server too old to tell: it ended.
				s.finish(io.EOF, "")
				return
			}
		}
		// Dropped as too far behind, or the link was lost: attach again,
		// and not over and over when the client keeps falling behind.
		if time.Since(since) < time.Second && !s.sleep(300*time.Millisecond) {
			return
		}
	}
}

func (s *Stream) sleep(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-s.done:
		return false
	}
}

func (s *Stream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// attachError is the server's refusal of an attachment, as a session
// that could not start.
type attachError struct{ msg string }

func (e *attachError) Error() string { return e.msg }

// attach attaches to the session at the terminal's size, once.
func (s *Stream) attach() (net.Conn, int, error) {
	tr, epoch, hello := s.c.transport()
	if tr == nil {
		return nil, epoch, ErrDisconnected
	}
	s.mu.Lock()
	a := Attach{
		Op: "attach", SID: s.sid, Cols: s.cols, Rows: s.rows,
		Scrollback: s.c.opts.Scrollback, Backlog: s.c.opts.Backlog,
	}
	// A session to create, at first or on another server than the one
	// attached to last, which lost it as it restarted. On the same one, a
	// session gone was ended by another window.
	if s.create != nil && (s.server == "" || s.server != hello.Instance()) {
		o := *s.create
		a.Create = &o
	}
	s.mu.Unlock()
	conn, err := tr.Dial()
	if err != nil {
		return nil, epoch, err
	}
	b, _ := json.Marshal(a)
	conn.SetDeadline(time.Now().Add(callTimeout + 10*time.Second))
	if _, err = conn.Write(append(b, '\n')); err == nil {
		err = readOK(conn)
	}
	conn.SetDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, epoch, err
	}
	s.mu.Lock()
	s.server = hello.Instance()
	s.sentCols, s.sentRows = a.Cols, a.Rows
	s.mu.Unlock()
	return conn, epoch, nil
}

// publish makes conn the stream's connection, sending what was typed
// meanwhile.
func (s *Stream) publish(conn net.Conn, reset bool) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	s.conn = conn
	s.gen++
	s.reset = s.reset || reset
	pending := s.pending
	s.pending = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	if len(pending) > 0 {
		conn.Write(pending)
	}
}

// finish ends the stream: reads return err once they returned failure.
func (s *Stream) finish(err error, failure string) {
	s.mu.Lock()
	s.end, s.failure = err, failure
	s.conn = nil
	s.cond.Broadcast()
	s.mu.Unlock()
}

// sync tells the server the terminal's size, once attached, when it was
// told another.
func (s *Stream) sync() error {
	s.mu.Lock()
	if s.conn == nil || s.cols == s.sentCols && s.rows == s.sentRows {
		s.mu.Unlock()
		return nil
	}
	cols, rows := s.cols, s.rows
	s.sentCols, s.sentRows = cols, rows
	s.mu.Unlock()
	err := s.c.Resize(s.sid, cols, rows)
	if err != nil {
		// Told again as it attaches again.
		s.mu.Lock()
		if s.sentCols == cols && s.sentRows == rows {
			s.sentCols, s.sentRows = 0, 0
		}
		s.mu.Unlock()
	}
	return err
}

// readOK reads the server's answer to an attach, a byte at a time so as
// to read nothing past it.
func readOK(conn net.Conn) error {
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := conn.Read(b); err != nil {
			return err
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
		if len(line) > 4096 {
			return errors.New("rex: bad answer")
		}
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(line, &res); err != nil {
		return err
	}
	if !res.OK {
		if res.Error == errNoSession {
			return ErrNoSession
		}
		return &attachError{msg: res.Error}
	}
	return nil
}

func (s *Stream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		for s.conn == nil && s.end == nil && !s.closed {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if s.conn == nil {
			err := s.end
			if s.failure != "" && !s.failShown {
				s.failShown = true
				msg := "\x1b[31mCould not start a session: " + s.failure + "\x1b[0m\r\n"
				s.mu.Unlock()
				return copy(p, msg), nil
			}
			s.mu.Unlock()
			return 0, err
		}
		conn, gen := s.conn, s.gen
		if s.reset && len(p) >= len(resetScreen) {
			s.reset = false
			s.mu.Unlock()
			return copy(p, resetScreen), nil
		}
		s.mu.Unlock()
		n, err := conn.Read(p)
		if n > 0 {
			if s.OnData != nil {
				s.OnData(n)
			}
			return n, nil
		}
		if err != nil {
			s.mu.Lock()
			if s.gen == gen && s.conn == conn {
				s.conn = nil
				select {
				case s.lost <- struct{}{}:
				default:
				}
			}
			s.mu.Unlock()
		}
	}
}

// maxPending is how much typed input waits for an attachment.
const maxPending = 1 << 20

// Write sends what is typed to the session. While the stream attaches,
// it waits for it; while the server is out of reach it is dropped, as
// it would run later, unseen.
func (s *Stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	conn, closed, end := s.conn, s.closed, s.end
	if conn == nil && !closed && end == nil {
		if ok, _ := s.c.connected(); ok && len(s.pending)+len(p) <= maxPending {
			s.pending = append(s.pending, p...)
		}
	}
	s.mu.Unlock()
	if closed || end != nil {
		return 0, io.ErrClosedPipe
	}
	if conn != nil {
		// An error shows as the connection breaks, to the reader, which
		// attaches again.
		conn.Write(p)
	}
	return len(p), nil
}

// Resize tells the session the size of the screen, attaching first.
func (s *Stream) Resize(cols, rows int) error {
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	first := !s.resized
	s.resized = true
	s.mu.Unlock()
	if first {
		close(s.sized)
	}
	return s.sync()
}

// Close detaches; the session goes on.
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.conn
	s.conn = nil
	close(s.done)
	s.cond.Broadcast()
	s.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Size returns the size the session was last given.
func (s *Stream) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// Started reports whether the stream has attached once.
func (s *Stream) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen > 0
}
