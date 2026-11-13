package rex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Transport is a way to a server: connections to it, for as long as it
// lasts. The server's local socket lasts for ever; a link to another
// machine, until the network or the machine drops it.
type Transport interface {
	// Dial opens a connection to the server.
	Dial() (net.Conn, error)
	// Done is closed once the transport is lost; nil, it never is.
	Done() <-chan struct{}
	Close() error
}

// pinger is a transport that measures its round trip.
type pinger interface {
	Ping() (time.Duration, error)
}

// Connector makes a transport, each time a client connects.
type Connector func(ctx context.Context) (Transport, error)

// local is the transport of the server of this machine.
type local struct{}

func (local) Dial() (net.Conn, error) { return net.Dial("unix", SocketPath()) }
func (local) Done() <-chan struct{}   { return nil }
func (local) Close() error            { return nil }

// ErrDisconnected is the error of a request while the client has no
// connection to its server.
var ErrDisconnected = errors.New("rex: not connected to the server")

// ErrNoSession is the error about a session the server does not have.
var ErrNoSession = errors.New("rex: " + errNoSession)

// State is how a client is connected to its server.
type State int

const (
	// Connecting: the client is reaching the server.
	Connecting State = iota
	// Connected: requests go through.
	Connected
	// Waiting: the connection failed or was lost, and the client tries
	// again at RetryAt.
	Waiting
	// Stopped: the client was closed, or lost a connection it does not
	// make again.
	Stopped
	// GaveUp: the client failed to connect MaxRetries times in a row, and
	// tries again only when kicked (Kick, Reconnect).
	GaveUp
)

func (s State) String() string {
	return [...]string{"connecting", "connected", "waiting", "stopped", "gave up"}[s]
}

// Status is a client's state, for the app to show.
type Status struct {
	State State
	// Err is why the last connection failed or ended.
	Err error
	// RetryAt is when a waiting client tries again, and Attempts how many
	// times in a row it failed to connect since it last had a connection.
	RetryAt  time.Time
	Attempts int
	// RTT is the round trip of the link, smoothed, and Stalled tells that
	// a ping went unanswered for a while: the link may be gone.
	RTT     time.Duration
	Stalled bool
	// Since is when the state began.
	Since time.Time
}

// Client is a client of a session server: its control connection, and
// the connections of the sessions it attaches to.
type Client struct {
	connect   Connector
	reconnect bool
	opts      DialOptions

	mu    sync.Mutex
	ctl   *control // nil while not connected
	tr    Transport
	hello Hello
	// epoch counts the connections made.
	epoch   int
	status  Status
	changed chan struct{} // closed and made anew as the state changes
	closed  bool
	stop    chan struct{}
	kick    chan struct{}
	// What to tell the server once connected again: the sessions to end,
	// and the layout to keep.
	kills     map[string]bool
	layout    json.RawMessage
	pingStart time.Time
	lost      chan struct{}
}

// DialOptions configure a client that connects, and connects again.
type DialOptions struct {
	// OnChange is called, from a goroutine of the client, as its state
	// changes.
	OnChange func()
	// Scrollback and Backlog are those of the client's attachments: see
	// Attach.
	Scrollback, Backlog int
	// MaxRetries is how many times in a row the client fails to connect
	// before it gives up, until kicked: 0 never gives up.
	MaxRetries int
}

// Connect connects to the server of this machine, starting one in the
// background when none runs. A client of it does not connect again: the
// server keeps running and the app quits when it does not.
func Connect() (*Client, error) {
	conn, err := dialLocal(true)
	if err != nil {
		return nil, err
	}
	c := newClient(func(context.Context) (Transport, error) { return local{}, nil }, false, DialOptions{})
	c.tr, c.ctl = local{}, newControl(conn)
	c.epoch = 1
	c.status = Status{State: Connected, Since: time.Now()}
	go func() {
		<-c.ctl.done
		c.mu.Lock()
		c.ctl = nil
		c.setStatus(Status{State: Stopped, Err: errors.New("connection to the server lost")})
		c.mu.Unlock()
		close(c.lost)
	}()
	return c, nil
}

// dialLocal connects to the server of this machine, starting one when
// none runs and spawn allows.
func dialLocal(spawn bool) (net.Conn, error) {
	conn, err := net.Dial("unix", SocketPath())
	if err == nil || !spawn {
		return conn, err
	}
	if err := Spawn(); err != nil {
		return nil, fmt.Errorf("rex: starting the server: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(30 * time.Millisecond)
		conn, err = net.Dial("unix", SocketPath())
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("rex: connecting to the server: %w", err)
		}
	}
}

// Dial returns a client that connects through connect in the background,
// and connects again whenever it loses its connection, until it is
// closed: requests wait a while for the connection, and sessions attached
// to attach again.
func Dial(connect Connector, o DialOptions) *Client {
	c := newClient(connect, true, o)
	go c.run()
	return c
}

func newClient(connect Connector, reconnect bool, o DialOptions) *Client {
	return &Client{
		connect: connect, reconnect: reconnect, opts: o,
		changed: make(chan struct{}), stop: make(chan struct{}), kick: make(chan struct{}, 1),
		kills: map[string]bool{}, lost: make(chan struct{}),
		status: Status{State: Connecting, Since: time.Now()},
	}
}

// setStatus changes the state; c.mu is held.
func (c *Client) setStatus(st Status) {
	st.RTT = c.status.RTT
	if st.State != c.status.State || st.Since.IsZero() {
		st.Since = time.Now()
	} else {
		st.Since = c.status.Since
	}
	c.status = st
	close(c.changed)
	c.changed = make(chan struct{})
	if f := c.opts.OnChange; f != nil {
		go f()
	}
}

// Status returns the client's state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	st.Stalled = st.State == Connected && !c.pingStart.IsZero() && time.Since(c.pingStart) > stallAfter
	return st
}

// stallAfter is how long a ping goes unanswered before the link shows as
// stalled.
const stallAfter = 2500 * time.Millisecond

// Server returns the server's hello of the connection, and which
// connection it is.
func (c *Client) Server() (Hello, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello, c.epoch
}

// Reconnect drops the connection, if there is one, and connects again
// at once: for a link that stopped answering, rather than wait for the
// pings to give up on it.
func (c *Client) Reconnect() {
	c.mu.Lock()
	tr := c.tr
	c.mu.Unlock()
	if tr != nil && c.reconnect {
		tr.Close()
	}
	c.Kick()
}

// Kick has a waiting client try again now, and one that gave up try
// again, as many times as at first.
func (c *Client) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Closed is closed once the client will not connect again.
func (c *Client) Closed() <-chan struct{} { return c.lost }

// run connects, and connects again, until the client is closed.
func (c *Client) run() {
	defer close(c.lost)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-c.stop
		cancel()
	}()
	// fails counts the failures in a row, the lost connection among them,
	// for the backoff; retries the attempts that failed since it was lost,
	// for MaxRetries.
	fails, retries := 0, 0
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		attempts := c.status.Attempts
		c.setStatus(Status{State: Connecting, Err: c.status.Err, Attempts: attempts})
		c.mu.Unlock()

		tr, ctl, h, err := c.dial(ctx)
		if err != nil {
			retries++
		} else {
			fails, retries = 0, 0
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				ctl.conn.Close()
				tr.Close()
				return
			}
			c.tr, c.ctl, c.hello = tr, ctl, h
			c.epoch++
			c.setStatus(Status{State: Connected})
			c.mu.Unlock()
			go c.flush(ctl)
			stopPing := make(chan struct{})
			if p, ok := tr.(pinger); ok {
				go c.ping(p, tr, stopPing)
			}
			select {
			case <-ctl.done:
			case <-tr.Done():
			case <-c.stop:
			}
			close(stopPing)
			ctl.conn.Close()
			tr.Close()
			err = errors.New("the connection was lost")
			if e := ctl.failure(); e != nil && !errors.Is(e, net.ErrClosed) {
				err = fmt.Errorf("the connection was lost: %w", e)
			}
			if r, ok := tr.(interface{ Reason() error }); ok && r.Reason() != nil {
				err = r.Reason()
			}
		}
		c.mu.Lock()
		c.ctl, c.tr = nil, nil
		if c.closed {
			c.mu.Unlock()
			return
		}
		fails++
		if max := c.opts.MaxRetries; max > 0 && retries >= max {
			c.setStatus(Status{State: GaveUp, Err: err, Attempts: retries})
			c.mu.Unlock()
			select {
			case <-c.kick:
				fails, retries = 0, 0
			case <-c.stop:
				return
			}
			continue
		}
		delay := backoff(fails, err)
		c.setStatus(Status{State: Waiting, Err: err, RetryAt: time.Now().Add(delay), Attempts: retries})
		c.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-c.kick:
		case <-c.stop:
			return
		}
	}
}

// backoff is how long to wait after the nth failure in a row: a moment
// after losing a connection that worked, then longer and longer, with a
// little chance in it so that clients do not all try at once.
func backoff(n int, err error) time.Duration {
	if errors.As(err, new(*MissingError)) {
		return time.Minute // nothing to connect to until GoRex is there
	}
	d := 250 * time.Millisecond << min(n-1, 8)
	d = min(d, 30*time.Second)
	return d + time.Duration(rand.Int64N(int64(d/4)+1))
}

// dial makes a transport, opens the control connection and says hello.
func (c *Client) dial(ctx context.Context) (Transport, *control, Hello, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tr, err := c.connect(cctx)
	if err != nil {
		return nil, nil, Hello{}, err
	}
	conn, err := tr.Dial()
	if err != nil {
		tr.Close()
		return nil, nil, Hello{}, err
	}
	ctl := newControl(conn)
	var h Hello
	if err := ctl.call(Request{Op: "hello"}, &h, 20*time.Second); err != nil {
		conn.Close()
		tr.Close()
		return nil, nil, Hello{}, err
	}
	if h.Version != ProtocolVersion {
		conn.Close()
		tr.Close()
		return nil, nil, Hello{}, &VersionError{Version: h.Version, Platform: h.Platform}
	}
	return tr, ctl, h, nil
}

// VersionError is the error of a server of another protocol.
type VersionError struct {
	Version  int
	Platform string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("the server speaks version %d of GoRex's protocol, not %d: update GoRex there", e.Version, ProtocolVersion)
}

// ping measures the link's round trip every few seconds, and drops a
// link that no longer answers, sooner than the operating system would.
func (c *Client) ping(p pinger, tr Transport, stop chan struct{}) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		c.mu.Lock()
		c.pingStart = time.Now()
		c.mu.Unlock()
		rtt, err := p.Ping()
		c.mu.Lock()
		c.pingStart = time.Time{}
		if err == nil {
			if c.status.RTT == 0 {
				c.status.RTT = rtt
			} else {
				c.status.RTT = (c.status.RTT*7 + rtt*3) / 10
			}
		}
		c.mu.Unlock()
		if err != nil {
			tr.Close()
			return
		}
		select {
		case <-t.C:
		case <-stop:
			return
		}
	}
}

// pingEvery is how often a remote link is pinged.
const pingEvery = 4 * time.Second

// flush sends what waited for the connection.
func (c *Client) flush(ctl *control) {
	c.mu.Lock()
	kills := c.kills
	c.kills = map[string]bool{}
	layout := c.layout
	c.layout = nil
	c.mu.Unlock()
	for sid := range kills {
		if err := ctl.call(Request{Op: "kill", SID: sid}, nil, 20*time.Second); err != nil {
			c.queueKill(sid)
		}
	}
	if layout != nil {
		if err := ctl.call(Request{Op: "setLayout", Layout: layout}, nil, 20*time.Second); err != nil {
			c.queueLayout(layout)
		}
	}
}

func (c *Client) queueKill(sid string) {
	c.mu.Lock()
	c.kills[sid] = true
	c.mu.Unlock()
}

func (c *Client) queueLayout(l json.RawMessage) {
	c.mu.Lock()
	if c.layout == nil {
		c.layout = l
	}
	c.mu.Unlock()
}

// Executable returns the path of the running executable and when it was
// modified last.
func Executable() (string, time.Time) {
	exe, err := os.Executable()
	if err != nil {
		return "", time.Time{}
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	st, err := os.Stat(exe)
	if err != nil {
		return exe, time.Time{}
	}
	return exe, st.ModTime()
}

// Restart shuts the server down, ending its sessions, waits for it to
// exit, and connects to a new one.
func (c *Client) Restart(pid int) (*Client, error) {
	c.Shutdown()
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for pid > 0 && processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return Connect()
}

// control is a control connection: requests, and their answers.
type control struct {
	conn    net.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan Response
	err     error
	done    chan struct{}
}

func newControl(conn net.Conn) *control {
	k := &control{conn: conn, pending: map[int64]chan Response{}, done: make(chan struct{})}
	go k.read()
	return k
}

func (k *control) failure() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

func (k *control) read() {
	r := bufio.NewReaderSize(k.conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			k.mu.Lock()
			k.err = err
			for id, ch := range k.pending {
				ch <- Response{ID: id, Error: "connection to the server lost"}
				delete(k.pending, id)
			}
			k.mu.Unlock()
			close(k.done)
			return
		}
		var res Response
		if json.Unmarshal(line, &res) != nil {
			continue
		}
		k.mu.Lock()
		ch := k.pending[res.ID]
		delete(k.pending, res.ID)
		k.mu.Unlock()
		if ch != nil {
			ch <- res
		}
	}
}

// errLost is the error of a request whose connection was lost before its
// answer came: it may or may not have been done.
var errLost = errors.New("rex: connection to the server lost")

// call sends a request and decodes the response's data into out.
func (k *control) call(req Request, out any, timeout time.Duration) error {
	ch := make(chan Response, 1)
	k.mu.Lock()
	if k.err != nil {
		k.mu.Unlock()
		return errLost
	}
	k.nextID++
	req.ID = k.nextID
	k.pending[req.ID] = ch
	k.mu.Unlock()
	b, _ := json.Marshal(req)
	k.wmu.Lock()
	_, err := k.conn.Write(append(b, '\n'))
	k.wmu.Unlock()
	if err != nil {
		return errLost
	}
	var res Response
	select {
	case res = <-ch:
	case <-time.After(timeout):
		k.mu.Lock()
		delete(k.pending, req.ID)
		k.mu.Unlock()
		return fmt.Errorf("rex: %s timed out", req.Op)
	}
	if res.Error != "" {
		if res.Error == "connection to the server lost" {
			return errLost
		}
		if strings.HasPrefix(res.Error, errNoSession) {
			return fmt.Errorf("%w: %s", ErrNoSession, strings.TrimPrefix(res.Error, errNoSession+" "))
		}
		return errors.New(res.Error)
	}
	if out != nil && len(res.Data) > 0 {
		return json.Unmarshal(res.Data, out)
	}
	return nil
}

// callTimeout is how long a request waits for its answer.
const callTimeout = 15 * time.Second

// call sends a request on the connection, waiting up to wait for one
// when the client has none, as while it connects again.
func (c *Client) call(req Request, out any, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		c.mu.Lock()
		ctl, changed, closed := c.ctl, c.changed, c.closed
		c.mu.Unlock()
		if ctl != nil {
			return ctl.call(req, out, callTimeout)
		}
		left := time.Until(deadline)
		if closed || !c.reconnect || left <= 0 {
			return ErrDisconnected
		}
		select {
		case <-changed:
		case <-time.After(left):
		case <-c.stop:
		}
	}
}

func (c *Client) Hello() (h Hello, err error) {
	err = c.call(Request{Op: "hello"}, &h, 0)
	return
}

// List lists the sessions, or fails at once without a connection.
func (c *Client) List() (s []SessionInfo, err error) {
	err = c.call(Request{Op: "list"}, &s, 0)
	return
}

// Info describes a session.
func (c *Client) Info(sid string) (s SessionInfo, err error) {
	err = c.call(Request{Op: "info", SID: sid}, &s, 0)
	return
}

func (c *Client) Create(o CreateOptions) (s SessionInfo, err error) {
	err = c.call(Request{Op: "create", Create: &o}, &s, 10*time.Second)
	return
}

// Kill ends a session. Without a connection, a client that connects
// again ends it once connected.
func (c *Client) Kill(sid string) error {
	err := c.call(Request{Op: "kill", SID: sid}, nil, 0)
	if c.reconnect && (errors.Is(err, ErrDisconnected) || errors.Is(err, errLost)) {
		c.queueKill(sid)
		return nil
	}
	return err
}

func (c *Client) Resize(sid string, cols, rows int) error {
	return c.call(Request{Op: "resize", SID: sid, Cols: cols, Rows: rows}, nil, 0)
}

// Layout returns the layout the server keeps, waiting a while for a
// connection.
func (c *Client) Layout() (json.RawMessage, error) {
	var l json.RawMessage
	err := c.call(Request{Op: "getLayout"}, &l, 0)
	return l, err
}

// SetLayout has the server keep a layout. Without a connection, a client
// that connects again sends it once connected, unless another came since.
func (c *Client) SetLayout(l json.RawMessage) error {
	c.mu.Lock()
	c.layout = nil // this one is newer
	c.mu.Unlock()
	err := c.call(Request{Op: "setLayout", Layout: l}, nil, 0)
	if c.reconnect && (errors.Is(err, ErrDisconnected) || errors.Is(err, errLost)) {
		c.queueLayout(l)
		return nil
	}
	return err
}

func (c *Client) Shutdown() error { return c.call(Request{Op: "shutdown"}, nil, 0) }

// Close closes the client and its connection; the sessions go on.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	ctl, tr := c.ctl, c.tr
	close(c.stop)
	c.setStatus(Status{State: Stopped})
	c.mu.Unlock()
	if ctl != nil {
		ctl.conn.Close()
	}
	if tr != nil && c.reconnect {
		tr.Close()
	}
	return nil
}

// transport returns the client's transport and which connection it is
// of, or nil without one.
func (c *Client) transport() (Transport, int, Hello) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctl == nil {
		return nil, c.epoch, c.hello
	}
	return c.tr, c.epoch, c.hello
}

// connected reports whether the client has a connection, and its epoch.
func (c *Client) connected() (bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctl != nil, c.epoch
}

// waitChange waits for the client's state to change, or done.
func (c *Client) waitChange(done <-chan struct{}) {
	c.mu.Lock()
	ch := c.changed
	c.mu.Unlock()
	select {
	case <-ch:
	case <-done:
	case <-c.stop:
	case <-c.lost:
	}
}

// NewID returns a new ID of a session.
func NewID() string { return newID() }
