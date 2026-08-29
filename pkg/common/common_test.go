package common

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
)

// pipeConn returns one end of an in-memory connection, closed on cleanup.
func pipeConn(t *testing.T) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return a
}

func TestSessionStore_NewAndGet(t *testing.T) {
	store := NewSessionStore()
	session := store.New(pipeConn(t))

	got, ok := store.Get(session.ID)
	if !ok {
		t.Fatal("a session registered with New was not retrievable")
	}
	if got.ID != session.ID {
		t.Errorf("got session %s, want %s", got.ID, session.ID)
	}
	if !got.IsOpen() {
		t.Error("a new session should be open")
	}
}

func TestSessionStore_GetMissing(t *testing.T) {
	store := NewSessionStore()
	if _, ok := store.Get(uuid.New()); ok {
		t.Error("Get returned ok for an ID that was never registered")
	}
}

func TestSessionStore_NewFromStreamRejectsDuplicates(t *testing.T) {
	store := NewSessionStore()
	id := uuid.New()

	if _, err := store.NewFromStream(id, pipeConn(t)); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}

	// A peer reusing an ID must not silently replace the live session, which
	// would strand the connection the first one owns.
	if _, err := store.NewFromStream(id, pipeConn(t)); err == nil {
		t.Error("expected an error when reusing a session ID, got nil")
	}
}

// TestSessionStore_StoresAreIsolated is the property that lets a client and a
// server run in the same process. Both sides address sessions by the same
// UUIDs, so with a shared store each would find the other's session.
func TestSessionStore_StoresAreIsolated(t *testing.T) {
	serverSide := NewSessionStore()
	clientSide := NewSessionStore()

	session := serverSide.New(pipeConn(t))

	if _, ok := clientSide.Get(session.ID); ok {
		t.Fatal("a session registered in one store was visible in another")
	}

	// The other side registering the same ID must succeed, not collide.
	if _, err := clientSide.NewFromStream(session.ID, pipeConn(t)); err != nil {
		t.Errorf("the peer could not register its own view of the session: %v", err)
	}
}

func TestSession_CloseReapsFromStore(t *testing.T) {
	store := NewSessionStore()
	store.SetReapDelay(10 * time.Millisecond)

	session := store.New(pipeConn(t))
	session.Close()

	if session.IsOpen() {
		t.Error("a closed session should not report itself open")
	}

	// Sessions linger briefly so that in-flight frames do not become
	// "session not found"; after the delay they must actually be removed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := store.Get(session.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed session was never reaped from its store")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if n := store.Len(); n != 0 {
		t.Errorf("store still holds %d sessions after reaping", n)
	}
}

func TestSession_CloseIsSafeWithoutAStore(t *testing.T) {
	// Sessions built outside a store must not panic on Close.
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{ID: uuid.New(), Context: ctx, cancelFunc: cancel}
	s.Close()
}

// TestSessionStore_CloseAllClosesTheConnections is the reconnect case: a
// tunnel that dies takes its store with it, and the store going out of scope
// closes nothing.
func TestSessionStore_CloseAllClosesTheConnections(t *testing.T) {
	store := NewSessionStore()

	// The far ends are kept, because whether they notice the close is the
	// whole point: an unclosed socket is what leaks.
	var peers []net.Conn
	var sessions []*Session
	for range 3 {
		near, far := net.Pipe()
		t.Cleanup(func() {
			_ = near.Close()
			_ = far.Close()
		})
		peers = append(peers, far)
		sessions = append(sessions, store.New(near))
	}

	store.CloseAll()

	if store.Len() != 0 {
		t.Errorf("CloseAll left %d session(s) in the store; a reconnect would inherit sessions from a stream that no longer exists", store.Len())
	}

	for i, session := range sessions {
		if session.IsOpen() {
			t.Errorf("session %d was still marked open after CloseAll", i)
		}
		select {
		case <-session.Context.Done():
		default:
			t.Errorf("session %d's context was not cancelled by CloseAll, so the goroutine reading it never stops", i)
		}
		// Read in a goroutine: a connection CloseAll failed to close would
		// block here forever rather than failing the test. net.Pipe cannot
		// take a read deadline once either end is closed, which is the very
		// state being asserted.
		read := make(chan error, 1)
		go func() {
			_, err := peers[i].Read(make([]byte, 1))
			read <- err
		}()
		select {
		case err := <-read:
			if err == nil {
				t.Errorf("connection %d was still open after CloseAll; every reconnect would leak one socket per session", i)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("connection %d was never closed by CloseAll; every reconnect would leak one socket per session", i)
		}
	}
}

// TestSessionStore_CloseAllOnAnEmptyStore guards the ordinary case: a tunnel
// that never carried a connection must still tear down without complaint.
func TestSessionStore_CloseAllOnAnEmptyStore(t *testing.T) {
	NewSessionStore().CloseAll()
}

func TestParsePorts(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantSource int32
		wantHost   string
		wantPort   int32
		wantErr    bool
	}{
		{name: "single port", in: "8000", wantSource: 8000, wantHost: "localhost", wantPort: 8000},
		{name: "source and target", in: "80:8000", wantSource: 80, wantHost: "localhost", wantPort: 8000},
		{name: "source host and target", in: "80:example.internal:8000", wantSource: 80, wantHost: "example.internal", wantPort: 8000},
		{name: "empty", in: "", wantErr: true},
		{name: "non-numeric source", in: "http:8000", wantErr: true},
		{name: "non-numeric target", in: "80:http", wantErr: true},
		{name: "non-numeric target with host", in: "80:example.internal:http", wantErr: true},
		{name: "too many parts", in: "1:2:3:4", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePorts(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParsePorts(%q) succeeded, want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePorts(%q) failed: %v", tc.in, err)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %d, want %d", got.Source, tc.wantSource)
			}
			if got.TargetHost != tc.wantHost {
				t.Errorf("target host = %q, want %q", got.TargetHost, tc.wantHost)
			}
			if got.TargetPort != tc.wantPort {
				t.Errorf("target port = %d, want %d", got.TargetPort, tc.wantPort)
			}
		})
	}
}
