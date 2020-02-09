package common

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
)

type Session struct {
	Id   uuid.UUID
	Conn *net.Conn
	Buf  *bytes.Buffer
	Open bool
	Lock sync.Mutex
}

type RedirectRequest struct {
	Source int32
	Target int32
}

func NewSession(conn *net.Conn) *Session {
	r := &Session{
		Id:   uuid.New(),
		Conn: conn,
		Buf:  &bytes.Buffer{},
		Open: true,
	}
	ok, err := AddSession(r)
	if ok != true {
		log.Printf("%s; failed registering request: %v", r.Id.String(), err)
	}
	return r
}

func NewSessionFromStream(id *uuid.UUID, conn *net.Conn) *Session {
	r := &Session{
		Id:   *id,
		Conn: conn,
		Buf:  &bytes.Buffer{},
		Open: true,
	}
	ok, err := AddSession(r)
	if ok != true {
		log.Printf("%s; failed registering request: %v", r.Id.String(), err)
	}
	return r
}

func AddSession(r *Session) (bool, error) {
	if _, ok := GetSession(&r.Id); ok != false {
		return false, errors.New(fmt.Sprintf("Session %s already exists", r.Id.String()))
	}
	openSessions[r.Id.String()] = r
	return true, nil
}

func GetSession(id *uuid.UUID) (*Session, bool) {
	request, ok := openSessions[id.String()]
	return request, ok
}

type SessionPool map[string]*Session

var openSessions = SessionPool{}

func CloseSession(id uuid.UUID) (bool, error) {
	request, ok := openSessions[id.String()]
	if ok == false {
		return false, errors.New(fmt.Sprintf("id %v not found in open requests", id))
	}
	conn := *request.Conn
	_ = conn.Close()
	delete(openSessions, id.String())
	return true, nil
}

func ParsePorts(s string) (*RedirectRequest, error) {
	raw := strings.Split(s, ":")
	if len(raw) == 0 {
		return nil, errors.New(fmt.Sprintf("failed parsing redirect request: %s", s))
	}
	if len(raw) == 1 {
		p, err := strconv.ParseInt(raw[0], 10, 32)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("failed to parse port %s, %v", raw[0], err))
		}
		return &RedirectRequest{
			Source: int32(p),
			Target: int32(p),
		}, nil
	}
	if len(raw) == 2 {
		s, err := strconv.ParseInt(raw[0], 10, 32)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("failed to parse port %s, %v", raw[0], err))
		}
		t, err := strconv.ParseInt(raw[1], 10, 32)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("failed to parse port %s, %v", raw[1], err))
		}
		return &RedirectRequest{
			Source: int32(s),
			Target: int32(t),
		}, nil
	}
	return nil, errors.New(fmt.Sprintf("Error, bad tunnel format: %s", s))
}
