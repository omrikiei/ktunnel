package common

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"log"
	"net"
	"sync"
)

const (
	bufferSize = 32*1024
)

type Request struct {
	Id uuid.UUID
	Conn *net.Conn
	Buf *bytes.Buffer
	Open bool
	Lock *sync.Mutex
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
	return r
}

func AddRequest(r *Request) (bool, error) {
	if _, ok := GetRequest(&r.Id); ok != false {
		return false, errors.New(fmt.Sprintf("Request %s already exists", r.Id.String()))
	}
	openRequests[r.Id] = r
	return true, nil
}

func GetRequest(id *uuid.UUID) (*Request, bool){
	request, ok := openRequests[*id]
	return request, ok
}

type RequestPool map[uuid.UUID]*Request

var openRequests = RequestPool{}

func CloseRequest(id uuid.UUID) (bool, error) {
	request, ok := openRequests[id]
	if ok == false {
		return false, errors.New(fmt.Sprintf("id %v not found in open requests", id))
	}
	conn := *request.Conn
	_ = conn.Close()
	delete(openRequests, id)
	return true, nil
}

func StreamToByte(stream io.Reader) (int, *[]byte, error) {
	//TODO: figure out a way to wait for readability or alternatively stream through the tunnel
	buf := make([]byte, bufferSize)
	br, err := stream.Read(buf)
	res := buf[:br]
	return br, &res, err
}

func StreamToChan(stream io.Reader) (int, *[]byte, error) {
	buf := make([]byte, bufferSize)
	reader := bufio.NewReader(stream)
	for br, err := reader.Read(buf); err != io.EOF; {
		fmt.Println(br, err)
	}
	res := buf
	return 0, &res, nil
}