//go:build darwin || linux || windows

package rex

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/term"
)

// Bridge serves a client on another machine, through ssh, on its standard
// input and output: it carries the client's connections to this machine's
// server, many on one pipe, starting the server when none runs. It exits
// as the pipe closes, as when the link drops; the server and its sessions
// go on.
func Bridge() error {
	// The pipe is the protocol's alone: what would be logged goes to a
	// file, and a pseudo-terminal ssh gave is made raw before a word is
	// said, so that it carries bytes as they are.
	os.MkdirAll(Dir(), 0o700)
	if f, err := os.OpenFile(filepath.Join(Dir(), "bridge.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		log.SetOutput(f)
		defer f.Close()
	} else {
		log.SetOutput(io.Discard)
	}
	log.SetPrefix(fmt.Sprintf("[bridge %d] ", os.Getpid()))
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		if old, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, old)
		}
	}
	return bridge(os.Stdin, os.Stdout)
}

// bridge serves the bridge's protocol on in and out.
func bridge(in io.ReadCloser, out io.Writer) error {
	// The server, ready before the client asks anything of it.
	conn, err := dialLocal(true)
	if err != nil {
		fmt.Fprintf(out, "\r\n%s%s\r\n", errorBanner, strings.ReplaceAll(err.Error(), "\n", " "))
		return err
	}
	conn.Close()
	fmt.Fprintf(out, "\r\n%s%d %s/%s\r\n", bridgeBanner, ProtocolVersion, runtime.GOOS, runtime.GOARCH)
	probe := make([]byte, probeSize)
	if _, err := io.ReadFull(in, probe); err != nil {
		return err
	}
	if _, err := out.Write(probe); err != nil {
		return err
	}
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.StreamCloseTimeout = 30 * time.Second
	cfg.LogOutput = io.Discard
	sess, err := yamux.Server(&pipe{r: in, w: &coalescer{w: out}, c: in}, cfg)
	if err != nil {
		return err
	}
	log.Print("serving")
	defer log.Print("done")
	for {
		st, err := sess.Accept()
		if err != nil {
			return nil
		}
		go proxy(st)
	}
}

// proxy carries a connection of the client to the server, both ways.
func proxy(st net.Conn) {
	defer st.Close()
	conn, err := dialLocal(true)
	if err != nil {
		log.Print(err)
		return
	}
	defer conn.Close()
	var once sync.Once
	done := make(chan struct{})
	end := func() { once.Do(func() { close(done) }) }
	go func() {
		io.Copy(conn, st)
		end()
	}()
	go func() {
		io.Copy(st, conn)
		end()
	}()
	<-done
}
