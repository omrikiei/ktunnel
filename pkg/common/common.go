// Package common for shared functions and types
package common

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	BufferSize = 1024 * 3
)

// DefaultSessionReapDelay is how long a closed session stays in its store
// before being removed. Frames for a session can still be in flight when it
// closes, and dropping it immediately turns those into "session not found".
const DefaultSessionReapDelay = 5 * time.Second

// SessionStore holds the open sessions for one side of a tunnel.
//
// This is per-side rather than process-global: a client and a server running
// in the same process address sessions by the same UUIDs, so a shared store
// would have each side finding the other's session instead of its own. In
// production they are separate processes, but tests -- and any future
// in-process use -- need them isolated.
type SessionStore struct {
	sessions sync.Map

	// reapDelay is DefaultSessionReapDelay unless overridden; tests set it
	// low so they do not wait out the delay.
	reapDelay time.Duration
}

// NewSessionStore returns an empty store.
func NewSessionStore() *SessionStore {
	return &SessionStore{reapDelay: DefaultSessionReapDelay}
}

// SetReapDelay overrides how long closed sessions linger before removal.
func (s *SessionStore) SetReapDelay(d time.Duration) {
	s.reapDelay = d
}

// New registers a session for conn under a freshly generated ID.
func (s *SessionStore) New(conn net.Conn) *Session {
	return s.add(uuid.New(), conn)
}

// NewFromStream registers a session for conn under an ID chosen by the peer.
// It returns an error if that ID is already in use.
func (s *SessionStore) NewFromStream(id uuid.UUID, conn net.Conn) (*Session, error) {
	if _, ok := s.Get(id); ok {
		return nil, fmt.Errorf("session %s already exists", id.String())
	}
	return s.add(id, conn), nil
}

func (s *SessionStore) add(id uuid.UUID, conn net.Conn) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Session{
		ID:         id,
		Conn:       conn,
		Context:    ctx,
		cancelFunc: cancel,
		Buf:        bytes.Buffer{},
		Open:       true,
		store:      s,
	}
	s.sessions.Store(id, r)
	return r
}

// Get returns the session with the given ID, if it is still present.
func (s *SessionStore) Get(id uuid.UUID) (*Session, bool) {
	session, ok := s.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return session.(*Session), true
}

// Delete removes a session immediately.
func (s *SessionStore) Delete(id uuid.UUID) {
	s.sessions.Delete(id)
}

// Len reports how many sessions the store currently holds.
func (s *SessionStore) Len() int {
	n := 0
	s.sessions.Range(func(_, _ interface{}) bool {
		n++
		return true
	})
	return n
}

type Session struct {
	ID         uuid.UUID
	Conn       net.Conn
	Buf        bytes.Buffer
	Context    context.Context
	cancelFunc context.CancelFunc
	Open       bool
	store      *SessionStore
	sync.Mutex
}

// IsOpen reports whether the session is still open.
func (s *Session) IsOpen() bool {
	s.Lock()
	defer s.Unlock()
	return s.Open
}

// SetOpen sets the session's open flag.
func (s *Session) SetOpen(open bool) {
	s.Lock()
	defer s.Unlock()
	s.Open = open
}

func (s *Session) Close() {
	s.cancelFunc()

	s.Lock()
	if s.Conn != nil {
		_ = s.Conn.Close()
		s.Open = false
	}
	s.Unlock()

	if s.store == nil {
		return
	}
	store, id := s.store, s.ID
	go func() {
		<-time.After(store.reapDelay)
		store.Delete(id)
	}()
}

type RedirectRequest struct {
	Source     int32
	TargetHost string
	TargetPort int32
}

func ParsePorts(s string) (*RedirectRequest, error) {
	raw := strings.Split(s, ":")
	if len(raw) == 0 {
		return nil, fmt.Errorf("failed parsing redirect request: %s", s)
	}
	if len(raw) == 1 {
		p, err := strconv.ParseInt(raw[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port %s, %v", raw[0], err)
		}
		return &RedirectRequest{
			Source:     int32(p),
			TargetHost: "localhost",
			TargetPort: int32(p),
		}, nil
	}
	if len(raw) == 2 {
		s, err := strconv.ParseInt(raw[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port %s, %v", raw[0], err)
		}
		t, err := strconv.ParseInt(raw[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port %s, %v", raw[1], err)
		}
		return &RedirectRequest{
			Source:     int32(s),
			TargetHost: "localhost",
			TargetPort: int32(t),
		}, nil
	}
	if len(raw) == 3 {
		s, err := strconv.ParseInt(raw[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port %s, %v", raw[0], err)
		}
		t, err := strconv.ParseInt(raw[2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port %s, %v", raw[1], err)
		}
		return &RedirectRequest{
			Source:     int32(s),
			TargetHost: raw[1],
			TargetPort: int32(t),
		}, nil
	}
	return nil, fmt.Errorf("bad tunnel format: %s", s)
}
