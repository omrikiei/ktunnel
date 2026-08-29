package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/common"
)

// TestReadConn_StopsWhenTheTunnelEnds is the server-side twin of the leak the
// client had.
//
// Every connection accepted on a tunnelled port gets a reader that hands the
// session to SendData over an unbuffered channel. SendData returns when its
// stream ends -- which, with reconnects, now happens every time a client's
// network blinks rather than once at the end of the process's life. The reader
// used to block on that send forever, holding the socket and its goroutine,
// and a server pod stays up for days.
func TestReadConn_StopsWhenTheTunnelEnds(t *testing.T) {
	conf, err := processArgs([]Option{WithSessionStore(common.NewSessionStore())})
	if err != nil {
		t.Fatalf("failed building a server config: %v", err)
	}

	peer, conn := net.Pipe()
	defer func() { _ = peer.Close() }()
	session := conf.sessions.New(conn)

	// Unbuffered and unread, which is what the channel becomes the moment
	// SendData returns.
	sessions := make(chan *common.Session)
	ctx, endStream := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		readConn(ctx, conf, session, sessions)
	}()

	// The reader is parked on its first send, with the connection open.
	select {
	case <-done:
		t.Fatal("readConn returned before its session was ever handed over")
	case <-time.After(100 * time.Millisecond):
	}

	// The tunnel ends.
	endStream()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readConn never returned after its tunnel ended; it holds a goroutine and an open socket per session, " +
			"and leaks another set every time a client reconnects")
	}

	if session.IsOpen() {
		t.Error("the session was left open after its tunnel ended, so the server keeps a connection nothing can read or write")
	}
	// The socket itself, from the other end: a read on a connection whose
	// server side has gone away must not block. (A deadline cannot be used
	// to bound this -- net.Pipe refuses to set one once either end is
	// closed, which is the very state under test.)
	readErr := make(chan error, 1)
	go func() {
		_, err := peer.Read(make([]byte, 1))
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Error("a read on the far end of the connection succeeded after its tunnel ended")
		}
	case <-time.After(5 * time.Second):
		t.Error("the connection was still open after its tunnel ended; whatever is on the other end waits on a tunnel that is gone")
	}
}
