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

type Request struct {
	Id uuid.UUID
	Conn *net.Conn
	Buf *bytes.Buffer
	Open bool
	Lock *sync.Mutex
}

type RedirectRequest struct {
	Source int32
	Target int32
}

func NewRequest(conn *net.Conn) *Request {
	r := &Request{
		Id:   uuid.New(),
		Conn:    conn,
		Buf:   &bytes.Buffer{},
		Open: true,
		Lock: &sync.Mutex{},
	}
	ok, err := AddRequest(r)
	if ok != true {
		log.Printf("%s; failed registering request: %v", r.Id.String(), err)
	}
	return r
}

func NewRequestFromStream(id *uuid.UUID, conn *net.Conn) *Request {
	r := &Request{
		Id:   *id,
		Conn:    conn,
		Buf:   &bytes.Buffer{},
		Open: true,
		Lock: &sync.Mutex{},
	}
	ok, err := AddRequest(r)
	if ok != true {
		log.Printf("%s; failed registering request: %v", r.Id.String(), err)
	}
	return r
}

func AddRequest(r *Request) (bool, error) {
	if _, ok := GetRequest(&r.Id); ok != false {
		return false, errors.New(fmt.Sprintf("Request %s already exists", r.Id.String()))
	}
	openRequests[r.Id.String()] = r
	return true, nil
}

func GetRequest(id *uuid.UUID) (*Request, bool){
	request, ok := openRequests[id.String()]
	return request, ok
}

type RequestPool map[string]*Request

var openRequests = RequestPool{}

func CloseRequest(id uuid.UUID) (bool, error) {
	request, ok := openRequests[id.String()]
	if ok == false {
		return false, errors.New(fmt.Sprintf("id %v not found in open requests", id))
	}
	conn := *request.Conn
	_ = conn.Close()
	delete(openRequests, id.String())
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