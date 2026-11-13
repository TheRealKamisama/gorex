package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"gorex/internal/rex"
)

// Tab is a tab of the window: panes in a tree of splits.
type Tab struct {
	ID   int
	host *Host
	Name string // the name the user gave it, "" to follow its panes
	Root *Node
	// Focus is the pane with the focus, and Zoom the pane that fills the
	// tab, when one does.
	Focus *Pane
	Zoom  *Pane
	// recent lists the panes by when they had the focus last, the latest
	// last: closing a pane gives the focus back to the one before.
	recent []*Pane
}

// Node is a pane, or a split of two nodes.
type Node struct {
	ID     int
	Pane   *Pane
	Parent *Node
	// Vertical stacks A above B; otherwise A is left of B. Ratio is A's
	// share of the room.
	Vertical bool
	Ratio    float32
	A, B     *Node
}

// Pane is a terminal attached to a session of the server.
type Pane struct {
	ID     int
	SID    string
	host   *Host
	Tab    *Tab
	Node   *Node
	term   *terminal.Terminal
	stream *rex.Stream

	// info is what the server last said of the session.
	info rex.SessionInfo
	// title is the last title the program set.
	title string
	// lastData is when the session last printed, in Unix nanoseconds.
	lastData atomic.Int64
	// attention marks a pane that rang the bell or whose program finished
	// while it was out of sight; seen clears it.
	attention bool
	// bounds is where the pane's card was in the last frame.
	bounds ui.Rect
	closed bool
	// restored tells that the session was made anew for a pane of a
	// saved layout whose session was gone.
	restored bool
	startDir string
}

// App is the state of GoRex's window.
type App struct {
	win *mygo.Window
	err string

	// hosts are the machines whose tabs the window shows, this one first;
	// host is the one it shows now.
	hosts  []*Host
	host   *Host
	nextID int

	// focusReq asks the view to give the keyboard focus to a pane's
	// terminal.
	focusReq *Pane

	// The command palette, the host's popover and the tab being renamed.
	paletteOpen  bool
	paletteQuery string
	paletteSel   int
	hostOpen     bool
	renaming     *Tab
	renameText   string
	renameFocus  bool
	saveDue      bool
	lastSave     time.Time
	quitting     bool
	focusedWin   bool
	title        string

	// The dialog that adds a remote host.
	addOpen    bool
	addDest    string
	addName    string
	addCmd     string
	addRetries string
	addErr     string
	addFocus   bool

	// posted are changes to make in the next frame, from terminals of a
	// view without a window, as in tests.
	postMu sync.Mutex
	posted []func()
	// services keeps redraw requests available to menu callbacks.
	services ui.Services
}

func (a *App) post(fn func()) {
	a.postMu.Lock()
	a.posted = append(a.posted, fn)
	a.postMu.Unlock()
}

// later changes the tree of tabs and panes once the frame building now
// is built, as closing what it is building would pull it from under it.
func (a *App) later(c *ui.Context, fn func()) {
	a.post(fn)
	c.Invalidate()
}

// runPosted makes the changes posted since the last frame.
func (a *App) runPosted() {
	a.postMu.Lock()
	fns := a.posted
	a.posted = nil
	a.postMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (a *App) id() int {
	a.nextID++
	return a.nextID
}

func (a *App) tab() *Tab { return a.host.tab() }

// panes returns the panes of a node in order, left to right and top to
// bottom.
func (n *Node) panes() []*Pane {
	if n == nil {
		return nil
	}
	if n.Pane != nil {
		return []*Pane{n.Pane}
	}
	return append(n.A.panes(), n.B.panes()...)
}

func (t *Tab) panes() []*Pane { return t.Root.panes() }

// setFocus gives a pane the focus of its tab.
func (t *Tab) setFocus(p *Pane) {
	if p == nil {
		return
	}
	t.Focus = p
	p.attention = false
	t.recent = slices.DeleteFunc(t.recent, func(q *Pane) bool { return q == p })
	t.recent = append(t.recent, p)
}

// newPane makes a pane with a new session in dir, on the host shown.
func (a *App) newPane(dir string, cols, rows int) *Pane { return a.newPaneOn(a.host, dir, cols, rows) }

// newPaneOn makes a pane with a new session in dir, on a host. A remote
// host's session starts as the pane attaches, without waiting for it, in
// one round trip; "" is the home directory there.
func (a *App) newPaneOn(h *Host, dir string, cols, rows int) *Pane {
	if cols <= 0 {
		cols, rows = 80, 24
	}
	if h.isRemote() {
		o := rex.CreateOptions{ID: rex.NewID(), Dir: dir, Cols: cols, Rows: rows}
		p := &Pane{ID: a.id(), SID: o.ID, host: h, info: rex.SessionInfo{ID: o.ID, Dir: dir}, startDir: dir}
		a.attachStream(p, h.client.StreamCreate(o, cols, rows), cols, rows)
		return p
	}
	info, err := h.client.Create(rex.CreateOptions{Dir: dir, Cols: cols, Rows: rows})
	if err != nil {
		log.Printf("creating a session: %v", err)
		a.err = err.Error()
		info = rex.SessionInfo{ID: "", Exited: true}
	}
	p := &Pane{ID: a.id(), SID: info.ID, host: h, info: info, startDir: dir}
	if p.info.Dir == "" {
		p.info.Dir = dir
	}
	a.attach(p, cols, rows)
	return p
}

// attach makes the pane's terminal, attached to its session. A remote
// one's session starts again, in its directory, should the host's server
// have lost it as it restarted.
func (a *App) attach(p *Pane, cols, rows int) {
	if p.SID == "" {
		term, err := terminal.New(terminal.Options{Conn: nopConn{}, Transparent: true, Font: termFont, Theme: lightTerm, DarkTheme: darkTerm})
		if err == nil {
			term.Feed([]byte("\x1b[31mCould not start a session: " + a.err + "\x1b[0m\r\n"))
			p.term = term
		}
		return
	}
	if p.host.isRemote() {
		dir := p.info.Dir
		if dir == "" {
			dir = p.startDir
		}
		a.attachStream(p, p.host.client.StreamCreate(rex.CreateOptions{ID: p.SID, Dir: dir, Cols: cols, Rows: rows}, cols, rows), cols, rows)
		return
	}
	a.attachStream(p, p.host.client.Stream(p.SID, cols, rows), cols, rows)
}

// attachStream makes the pane's terminal, on a stream of its session.
func (a *App) attachStream(p *Pane, s *rex.Stream, cols, rows int) {
	p.stream = s
	p.stream.OnData = func(int) { p.lastData.Store(time.Now().UnixNano()) }
	// The terminal's callbacks change the state on the main thread, in
	// the window that made the pane, as long as it is open.
	win := a.win
	update := func(fn func()) {
		if win != nil {
			win.Update(fn)
		} else {
			a.post(fn)
		}
	}
	term, err := terminal.New(terminal.Options{
		Conn:        p.stream,
		Font:        termFont,
		Theme:       lightTerm,
		DarkTheme:   darkTerm,
		Transparent: true,
		OnTitle: func(title string) {
			update(func() { p.title = title })
		},
		OnBell: func() {
			update(func() { a.ring(p) })
		},
		OnNotify: func(title, body string) {
			if title == "" {
				title = "GoRex"
			}
			mygo.NewNotification(mygo.NotificationOptions{Title: title, Body: body}).Show()
		},
		OnExit: func(int) {
			update(func() {
				// A session that could not start shows why, in its pane.
				if !p.closed && !a.quitting && !s.Failed() {
					a.closePane(p)
				}
			})
		},
	})
	if err != nil {
		log.Printf("terminal: %v", err)
		a.err = err.Error()
		return
	}
	// Until a view shows it, the terminal has the session's size, so that
	// attaching changes nothing in the session.
	term.Resize(cols, rows)
	p.term = term
}

// nopConn is the Conn of a terminal that only shows what the app feeds it.
type nopConn struct{}

func (nopConn) Read([]byte) (int, error)    { select {} }
func (nopConn) Write(p []byte) (int, error) { return len(p), nil }
func (nopConn) Close() error                { return nil }

func (a *App) ring(p *Pane) {
	if t := a.tab(); t == nil || t.Focus != p || p.Tab != t || !a.focusedWin {
		p.attention = true
	}
}

// newTab opens a tab with a shell in dir, after the active tab, on the
// host shown: none on a remote host before its tabs loaded.
func (a *App) newTab(dir string) *Tab { return a.newTabOn(a.host, dir) }

func (a *App) newTabOn(h *Host, dir string) *Tab {
	if !h.ready() {
		return nil
	}
	t := &Tab{ID: a.id(), host: h}
	p := a.newPaneOn(h, dir, 0, 0)
	t.Root = &Node{ID: a.id(), Pane: p}
	p.Tab, p.Node = t, t.Root
	t.setFocus(p)
	at := min(h.active+1, len(h.tabs))
	h.tabs = slices.Insert(h.tabs, at, t)
	h.active = at
	if a.host == h {
		a.focusReq = p
	}
	a.changed()
	return t
}

// currentDir returns the directory new panes open in: the focused pane's,
// or the home directory, "" on a remote host (whose server knows it).
func (a *App) currentDir() string {
	if t := a.tab(); t != nil && t.Focus != nil {
		if d := t.Focus.info.Dir; d != "" {
			return d
		}
	}
	if a.host.isRemote() {
		return ""
	}
	home, _ := os.UserHomeDir()
	return home
}

// split splits the focused pane, the new pane right of it or below it.
func (a *App) split(vertical bool) {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	old := t.Focus
	t.Zoom = nil
	n := old.Node
	cols, rows := 80, 24
	if old.term != nil {
		cols, rows = old.term.Size()
		if vertical {
			rows = max(rows/2, 2)
		} else {
			cols = max(cols/2, 10)
		}
	}
	p := a.newPaneOn(old.host, a.currentDir(), cols, rows)
	p.Tab = t
	left := &Node{ID: a.id(), Pane: old, Parent: n}
	right := &Node{ID: a.id(), Pane: p, Parent: n}
	old.Node, p.Node = left, right
	n.Pane, n.A, n.B, n.Vertical, n.Ratio = nil, left, right, vertical, 0.5
	t.setFocus(p)
	a.focusReq = p
	a.changed()
}

// closePane closes a pane and ends its session.
func (a *App) closePane(p *Pane) {
	if p.closed {
		return
	}
	p.closed = true
	if p.term != nil {
		p.term.Close()
	}
	if sid := p.SID; sid != "" && p.host.client != nil {
		c := p.host.client
		go c.Kill(sid)
	}
	t := p.Tab
	if t.Zoom == p {
		t.Zoom = nil
	}
	t.recent = slices.DeleteFunc(t.recent, func(q *Pane) bool { return q == p })
	n := p.Node
	if n.Parent == nil {
		a.removeTab(t)
		return
	}
	parent := n.Parent
	other := parent.A
	if other == n {
		other = parent.B
	}
	// The other side takes the parent's place.
	parent.Pane, parent.A, parent.B, parent.Vertical, parent.Ratio = other.Pane, other.A, other.B, other.Vertical, other.Ratio
	if parent.Pane != nil {
		parent.Pane.Node = parent
	} else {
		parent.A.Parent, parent.B.Parent = parent, parent
	}
	if t.Focus == p {
		next := (*Pane)(nil)
		if k := len(t.recent); k > 0 {
			next = t.recent[k-1]
		} else if ps := t.panes(); len(ps) > 0 {
			next = ps[0]
		}
		t.Focus = nil
		t.setFocus(next)
	}
	if a.tab() == t {
		a.focusReq = t.Focus
	}
	a.changed()
}

// removeTab removes a tab from its host. When it was the last tab of
// them all, the window closes; when the last of the host shown, another
// host's show.
func (a *App) removeTab(t *Tab) {
	h := t.host
	if h == nil {
		h = a.host
	}
	i := slices.Index(h.tabs, t)
	if i < 0 {
		return
	}
	h.tabs = slices.Delete(h.tabs, i, i+1)
	if h.active > i || h.active >= len(h.tabs) {
		h.active = max(h.active-1, 0)
	}
	if a.renaming == t {
		a.renaming = nil
	}
	if len(h.tabs) == 0 && h == a.host {
		for _, o := range a.hosts {
			if o != h && len(o.tabs) > 0 {
				a.changed()
				a.save()
				a.selectHost(o)
				return
			}
		}
		a.changed()
		a.save()
		if a.win != nil {
			a.win.Close()
		}
		return
	}
	if nt := a.tab(); nt != nil && h == a.host {
		a.focusReq = nt.Focus
	}
	a.changed()
}

// closeTab closes a tab and ends the sessions of its panes.
func (a *App) closeTab(t *Tab) {
	for _, p := range t.panes() {
		p.closed = true
		if p.term != nil {
			p.term.Close()
		}
		if p.SID != "" && p.host.client != nil {
			go p.host.client.Kill(p.SID)
		}
	}
	a.removeTab(t)
}

func (a *App) selectTab(i int) {
	if i < 0 || i >= len(a.host.tabs) {
		return
	}
	a.host.active = i
	t := a.host.tabs[i]
	if t.Focus != nil {
		t.Focus.attention = false
	}
	a.focusReq = t.Focus
	a.changed()
}

// moveFocus gives the focus to the pane next to the focused one, toward
// dx, dy.
func (a *App) moveFocus(dx, dy int) {
	t := a.tab()
	if t == nil || t.Focus == nil || t.Zoom != nil {
		return
	}
	from := t.Focus.bounds
	cx, cy := from.X+from.W/2, from.Y+from.H/2
	var best *Pane
	bestScore := float32(1e9)
	for _, p := range t.panes() {
		if p == t.Focus {
			continue
		}
		r := p.bounds
		var dist, overlap float32
		switch {
		case dx > 0:
			dist = r.X - (from.X + from.W)
			overlap = min(r.Y+r.H, from.Y+from.H) - max(r.Y, from.Y)
		case dx < 0:
			dist = from.X - (r.X + r.W)
			overlap = min(r.Y+r.H, from.Y+from.H) - max(r.Y, from.Y)
		case dy > 0:
			dist = r.Y - (from.Y + from.H)
			overlap = min(r.X+r.W, from.X+from.W) - max(r.X, from.X)
		default:
			dist = from.Y - (r.Y + r.H)
			overlap = min(r.X+r.W, from.X+from.W) - max(r.X, from.X)
		}
		if dist < -4 || overlap <= 0 {
			continue
		}
		// The nearest, then the one most in line with the focused pane's
		// center.
		px, py := r.X+r.W/2, r.Y+r.H/2
		off := px - cx
		if dx != 0 {
			off = py - cy
		}
		score := dist*4 + abs(off)
		if score < bestScore {
			best, bestScore = p, score
		}
	}
	if best != nil {
		t.setFocus(best)
		a.focusReq = best
	}
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// resizeFocused moves the nearest divider of the focused pane on the
// axis of dx, dy.
func (a *App) resizeFocused(dx, dy int) {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	vertical := dy != 0
	for n := t.Focus.Node; n.Parent != nil; n = n.Parent {
		p := n.Parent
		if p.Vertical != vertical {
			continue
		}
		step := float32(0.05)
		if dx < 0 || dy < 0 {
			step = -step
		}
		p.Ratio = min(max(p.Ratio+step, 0.1), 0.9)
		a.changed()
		return
	}
}

// equalize gives every split of the tab equal shares, by the panes on
// either side.
func (a *App) equalize() {
	t := a.tab()
	if t == nil {
		return
	}
	var walk func(n *Node) (cols, rows int)
	walk = func(n *Node) (int, int) {
		if n.Pane != nil {
			return 1, 1
		}
		ac, ar := walk(n.A)
		bc, br := walk(n.B)
		if n.Vertical {
			n.Ratio = float32(ar) / float32(ar+br)
			return max(ac, bc), ar + br
		}
		n.Ratio = float32(ac) / float32(ac+bc)
		return ac + bc, max(ar, br)
	}
	walk(t.Root)
	a.changed()
}

func (a *App) toggleZoom() {
	t := a.tab()
	if t == nil || t.Focus == nil {
		return
	}
	if t.Zoom != nil {
		t.Zoom = nil
	} else if len(t.panes()) > 1 {
		t.Zoom = t.Focus
	}
	a.focusReq = t.Focus
	a.changed()
}

// label returns how a pane names itself: its program, or the title the
// program set, and the directory it works in.
func (p *Pane) label() (name, detail string) {
	in := p.info
	prog := programOf(in.Program)
	home := p.home()
	dir := shortDirIn(in.Dir, home)
	if in.Program == "" {
		if in.Shell == "" {
			return "Shell", shortDirIn(p.startDir, home)
		}
		return in.Shell, shortDirIn(p.startDir, home)
	}
	if in.Idle || prog.Shell {
		name := in.Program
		if name == "" {
			name = in.Shell
		}
		return name, dir
	}
	if t := strings.TrimSpace(p.title); t != "" && !titleOfShell(t, in, home) {
		return t, ""
	}
	name = prog.Name
	if name == "ssh" || in.Program == "ssh" {
		if host := sshHost(in.Args); host != "" {
			return "SSH", host
		}
	}
	return name, dir
}

// home is the home directory of the pane's host.
func (p *Pane) home() string {
	if p.host != nil {
		return p.host.home()
	}
	home, _ := os.UserHomeDir()
	return home
}

// titleOfShell tells a title a shell set, naming the program or the
// directory, from one a program set of its own.
func titleOfShell(title string, in rex.SessionInfo, home string) bool {
	l := strings.ToLower(title)
	for _, w := range []string{strings.ToLower(in.Program), strings.ToLower(in.Shell)} {
		// Windows's consoles title themselves with the shell's file:
		// "C:\WINDOWS\system32\cmd.exe - ping".
		if w != "" && (strings.HasPrefix(l, w) || strings.Contains(l, `\`+w+".exe")) {
			return true
		}
	}
	if strings.Contains(title, "~") || (home != "" && strings.Contains(title, home)) {
		return true
	}
	return strings.Contains(title, "@") && strings.Contains(title, ":")
}

func sshHost(args []string) string {
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if len(a) == 2 && strings.Contains("bcDEeFIiJLlmOopQRSWw", a[1:]) {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// shortDir shows a directory of this machine as shells do, from ~, and
// only its last parts when it is deep.
func shortDir(d string) string {
	home, _ := os.UserHomeDir()
	return shortDirIn(d, home)
}

// shortDirIn shows a directory of a machine whose home is home, which
// may be another machine of other separators than this one's.
func shortDirIn(d, home string) string {
	if d == "" {
		return ""
	}
	sep := string(filepath.Separator)
	if strings.HasPrefix(d, "/") {
		sep = "/"
	}
	if home != "" {
		if samePath(d, home) {
			return "~"
		}
		if len(d) > len(home) && samePath(d[:len(home)], home) && string(d[len(home)]) == sep {
			d = "~" + d[len(home):]
		}
	}
	parts := strings.Split(d, sep)
	if len(parts) > 5 || len(d) > 48 && len(parts) > 3 {
		return "…" + sep + strings.Join(parts[len(parts)-2:], sep)
	}
	return d
}

// samePath reports whether two paths are the same, without case on
// Windows.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// tabLabel names a tab: its name, or its focused pane's.
func (t *Tab) label() (name, detail string) {
	if t.Name != "" {
		return t.Name, ""
	}
	if t.Focus == nil {
		return "Shell", ""
	}
	return t.Focus.label()
}

// The saved layout: the tabs, their splits and the sessions of their
// panes, which the server keeps, and the app restores when it starts.
type savedLayout struct {
	Active int        `json:"active"`
	Tabs   []savedTab `json:"tabs"`
}

type savedTab struct {
	Name  string     `json:"name,omitempty"`
	Root  *savedNode `json:"root"`
	Focus int        `json:"focus"`
	Zoom  bool       `json:"zoom,omitempty"`
}

type savedNode struct {
	SID      string     `json:"sid,omitempty"`
	Dir      string     `json:"dir,omitempty"`
	Cols     int        `json:"cols,omitempty"`
	Rows     int        `json:"rows,omitempty"`
	Vertical bool       `json:"vertical,omitempty"`
	Ratio    float32    `json:"ratio,omitempty"`
	A        *savedNode `json:"a,omitempty"`
	B        *savedNode `json:"b,omitempty"`
}

// snapshot is the layout of the host shown.
func (a *App) snapshot() savedLayout { return a.snapshotOf(a.host) }

func (a *App) snapshotOf(h *Host) savedLayout {
	l := savedLayout{Active: h.active}
	var save func(n *Node) *savedNode
	save = func(n *Node) *savedNode {
		if p := n.Pane; p != nil {
			s := &savedNode{SID: p.SID, Dir: p.info.Dir}
			if s.Dir == "" {
				s.Dir = p.startDir
			}
			if p.term != nil {
				s.Cols, s.Rows = p.term.Size()
			}
			return s
		}
		return &savedNode{Vertical: n.Vertical, Ratio: n.Ratio, A: save(n.A), B: save(n.B)}
	}
	for _, t := range h.tabs {
		st := savedTab{Name: t.Name, Root: save(t.Root), Zoom: t.Zoom != nil}
		st.Focus = max(slices.Index(t.panes(), t.Focus), 0)
		l.Tabs = append(l.Tabs, st)
	}
	return l
}

// changed notes that the layout changed, to save it soon.
func (a *App) changed() { a.saveDue = true }

// save sends the layout of each host to its server, which keeps it on
// disk.
func (a *App) save() {
	a.saveDue = false
	for _, h := range a.hosts {
		a.saveHost(h)
	}
}

// restore opens the tabs of this machine's saved layout.
func (a *App) restore() bool {
	h := a.local()
	raw, err := h.client.Layout()
	if err != nil {
		return false
	}
	infos, _ := h.client.List()
	return a.restoreHost(h, raw, infos)
}

// restoreHost opens the tabs of a host's saved layout, attaching to the
// sessions its server still has and starting new ones in place of those
// it lost: it reports whether it opened any.
func (a *App) restoreHost(h *Host, raw json.RawMessage, infos []rex.SessionInfo) bool {
	h.loaded = true
	var l savedLayout
	if len(raw) > 0 {
		json.Unmarshal(raw, &l)
	}
	live := map[string]rex.SessionInfo{}
	for _, in := range infos {
		if !in.Exited {
			live[in.ID] = in
		}
	}
	used := map[string]bool{}
	var load func(s *savedNode, t *Tab, parent *Node) *Node
	load = func(s *savedNode, t *Tab, parent *Node) *Node {
		n := &Node{ID: a.id(), Parent: parent}
		if s.A == nil || s.B == nil {
			var p *Pane
			if in, ok := live[s.SID]; ok && !used[s.SID] {
				used[s.SID] = true
				p = &Pane{ID: a.id(), SID: s.SID, host: h, info: in, startDir: s.Dir}
				a.attach(p, in.Cols, in.Rows)
			} else {
				dir := s.Dir
				if h.isRemote() {
					// The server starts it at home when it is gone.
				} else if st, err := os.Stat(dir); err != nil || !st.IsDir() {
					dir, _ = os.UserHomeDir()
				}
				p = a.newPaneOn(h, dir, s.Cols, s.Rows)
				p.restored = true
			}
			p.Tab, p.Node = t, n
			n.Pane = p
			return n
		}
		n.Vertical, n.Ratio = s.Vertical, min(max(s.Ratio, 0.05), 0.95)
		n.A, n.B = load(s.A, t, n), load(s.B, t, n)
		return n
	}
	for _, st := range l.Tabs {
		if st.Root == nil {
			continue
		}
		t := &Tab{ID: a.id(), Name: st.Name, host: h}
		t.Root = load(st.Root, t, nil)
		ps := t.panes()
		for _, p := range ps {
			t.setFocus(p)
		}
		t.setFocus(ps[min(max(st.Focus, 0), len(ps)-1)])
		if st.Zoom && len(ps) > 1 {
			t.Zoom = t.Focus
		}
		h.tabs = append(h.tabs, t)
	}
	// Sessions no tab shows, as those of another window that crashed,
	// get a tab of their own rather than running unseen.
	for _, in := range sortedInfos(live) {
		if used[in.ID] {
			continue
		}
		t := &Tab{ID: a.id(), host: h}
		p := &Pane{ID: a.id(), SID: in.ID, host: h, info: in, startDir: in.Dir}
		a.attach(p, in.Cols, in.Rows)
		t.Root = &Node{ID: a.id(), Pane: p}
		p.Tab, p.Node = t, t.Root
		t.setFocus(p)
		h.tabs = append(h.tabs, t)
	}
	if len(h.tabs) == 0 {
		return false
	}
	h.active = min(max(l.Active, 0), len(h.tabs)-1)
	if h == a.host {
		a.focusReq = h.tab().Focus
	}
	return true
}

func sortedInfos(m map[string]rex.SessionInfo) []rex.SessionInfo {
	out := make([]rex.SessionInfo, 0, len(m))
	for _, in := range m {
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b rex.SessionInfo) int { return a.Created.Compare(b.Created) })
	return out
}

// poll asks this machine's server what runs in each session, every half
// second, until the window closes.
func (a *App) poll(win *mygo.Window, h *Host) {
	client := h.client
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-client.Closed():
			win.Update(func() {
				if !a.quitting {
					a.err = "Lost the connection to the session server."
				}
			})
			return
		case <-tick.C:
		}
		infos, err := client.List()
		if err != nil {
			continue
		}
		byID := make(map[string]rex.SessionInfo, len(infos))
		for _, in := range infos {
			byID[in.ID] = in
		}
		win.Update(func() {
			if a.win == win {
				a.apply(h, byID)
			}
		})
	}
}

// apply takes what a host's server said of its sessions.
func (a *App) apply(h *Host, byID map[string]rex.SessionInfo) {
	for ti, t := range h.tabs {
		for _, p := range t.panes() {
			in, ok := byID[p.SID]
			if !ok {
				continue
			}
			was := p.info
			p.info = in
			if p.stream != nil && in.Dir != "" && was.Dir != in.Dir {
				p.stream.SetDir(in.Dir)
			}
			// A program that ran a while and finished out of sight
			// asks for attention.
			if !was.Idle && was.Program != "" && in.Idle && was.Program != in.Program {
				seen := h == a.host && ti == h.active && t.Focus == p && a.focusedWin
				if !seen {
					p.attention = true
				}
				if !a.focusedWin && time.Since(was.LastInput) > 8*time.Second {
					prog := programOf(was.Program)
					body := "in " + shortDirIn(in.Dir, h.home())
					if h.isRemote() {
						body += " on " + h.name()
					}
					n := mygo.NewNotification(mygo.NotificationOptions{
						Title: prog.Name + " finished",
						Body:  body,
					})
					n.Show()
				}
			}
			if was.Dir != in.Dir {
				a.changed()
			}
		}
	}
	if a.saveDue && time.Since(a.lastSave) > time.Second {
		a.save()
	}
}

// laterService queues a menu action without keeping a build context.
func (a *App) laterService(fn func()) { a.post(fn); a.services.Invalidate() }
