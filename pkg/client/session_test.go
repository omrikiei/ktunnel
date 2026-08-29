package client

import (
	"net"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/common"
)

// TestSendSession_ReleasesWhenTheSessionCloses is the regression test for the
// leak that only bites once tunnels are rebuilt.
//
// SendData is the only receiver on this channel, and it is gone by the time a
// tunnel is being torn down. Every session's reader used to park on a bare
// send with nobody to take it, holding its socket for the life of the
// process -- one more set for every reconnect.
func TestSendSession_ReleasesWhenTheSessionCloses(t *testing.T) {
	store := common.NewSessionStore()
	near, far := net.Pipe()
	t.Cleanup(func() {
		_ = near.Close()
		_ = far.Close()
	})
	session := store.New(near)

	// Unbuffered and never read, standing in for a sender that has returned.
	sessionsOut := make(chan *common.Session)

	sent := make(chan bool, 1)
	go func() { sent <- sendSession(session, sessionsOut) }()

	session.Close()

	select {
	case ok := <-sent:
		if ok {
			t.Fatal("sendSession claimed it handed off a session that nobody received")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sendSession never returned after its session was closed; " +
			"the goroutine and the socket it holds are pinned for the life of the process, once per session per reconnect")
	}
}

// TestSendSession_DeliversWhileTheSessionIsOpen keeps the other half: the
// guard must not cost us the data path it protects.
func TestSendSession_DeliversWhileTheSessionIsOpen(t *testing.T) {
	store := common.NewSessionStore()
	near, far := net.Pipe()
	t.Cleanup(func() {
		_ = near.Close()
		_ = far.Close()
	})
	session := store.New(near)

	sessionsOut := make(chan *common.Session, 1)
	if !sendSession(session, sessionsOut) {
		t.Fatal("sendSession dropped a session on an open channel; bytes read from the local service would never reach the tunnel")
	}
	select {
	case got := <-sessionsOut:
		if got != session {
			t.Fatalf("sendSession delivered session %s, want %s", got.ID, session.ID)
		}
	default:
		t.Fatal("sendSession reported a successful hand-off but delivered nothing")
	}
}
