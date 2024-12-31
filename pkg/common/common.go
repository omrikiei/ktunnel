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

var openSessions = sync.Map{}

type Session struct {
	ID         uuid.UUID
	Conn       net.Conn
	Buf        bytes.Buffer
	Context    context.Context
	cancelFunc context.CancelFunc
	Open       bool
	sync.Mutex
}

func (s *Session) Close() {
	s.cancelFunc()
	if s.Conn != nil {
		_ = s.Conn.Close()
		s.Open = false
	}
	go func() {
		<-time.After(5 * time.Second)
		openSessions.Delete(s.ID)
	}()
}

type RedirectRequest struct {
	Source     int32
	TargetHost string
	TargetPort int32
}

func NewSession(conn net.Conn) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Session{
		ID:         uuid.New(),
		Conn:       conn,
		Context:    ctx,
		cancelFunc: cancel,
		Buf:        bytes.Buffer{},
		Open:       true,
	}
	_, _ = addSession(r)
	return r
}

func NewSessionFromStream(id uuid.UUID, conn net.Conn) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Session{
		ID:         id,
		Conn:       conn,
		Context:    ctx,
		cancelFunc: cancel,
		Buf:        bytes.Buffer{},
		Open:       true,
	}
	_, _ = addSession(r)
	return r
}

func addSession(r *Session) (bool, error) {
	if _, ok := GetSession(r.ID); ok {
		return false, fmt.Errorf("session %s already exists", r.ID.String())
	}
	openSessions.Store(r.ID, r)
	return true, nil
}

func GetSession(id uuid.UUID) (*Session, bool) {
	request, ok := openSessions.Load(id)
	if ok {
		return request.(*Session), ok
	}
	return nil, ok
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
