package server

import (
	"fmt"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"io"
	"ktunnel/pkg/common"
	pb "ktunnel/tunnel_pb"
	"net"
	"strings"
)

func startHttpTunnel(stream pb.Tunnel_InitTunnelServer, scheme *pb.TunnelScheme, port *int32) error {
	ln, err := net.Listen(strings.ToLower(scheme.String()), fmt.Sprintf(":%d", port))
	if err != nil {
		defer func() {
			log.Errorf("Failed listening on port %d: %v", port, err)
		}()
		_ = stream.Send(&pb.SocketDataResponse{
			HasErr: true,
			LogMessage: &pb.LogMessage{
				LogLevel:             pb.LogLevel_ERROR,
				Message:              fmt.Sprintf("failed opening listener type %s on port %d: %v", scheme.String(), port, err),
			},
		})
		return err
	}

	requests := make(chan *common.Request)
	closeChan := make(chan bool, 1)
	go func(close <-chan bool){
		<-close
		log.Infof("Closing conenction on port %d", port)
		_ = ln.Close()
	}(closeChan)

	go receiveData(&stream, closeChan)
	go sendData(&stream, requests, closeChan)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		// socket -> stream
		go func(conn net.Conn) {
			if err != nil {
				log.Errorf("Failed accepting connection on port %d", port)
				return
			}
			request := common.NewRequest(&conn)
			log.Debugf("got new request %s", request.Id.String())
			// Read from socket in a loop and push messages to the requests channel
			// If the socket is closed, signal the channel to close connection

			for {
				buff := make([]byte, bufferSize)
				br, err := conn.Read(buff)
				request.Lock.Lock()
				if err != nil {
					if err != io.EOF {
						log.Errorf("%s; failed to read from server socket: %v", request.Id.String(), err)
					}
					request.Open = false
					request.Lock.Unlock()
					requests <- request
					break
				} else {
					_, err := request.Buf.Write(buff[:br])
					if err != nil {
						log.Errorf("%s; failed to write to request buffer: %v", request.Id.String(), err)
					}
				}
				request.Lock.Unlock()
				requests <- request
			}
		}(conn)
	}
}

func sendData(stream *pb.Tunnel_InitTunnelServer, requests <-chan *common.Request, closeChan chan<- bool) {
	for {
		request := <-requests
		request.Lock.Lock()
		if request.Buf.Len() > 0 || request.Open == false {
			st := *stream
			resp := &pb.SocketDataResponse{
				HasErr:               false,
				LogMessage:           nil,
				RequestId:            request.Id.String(),
				Data:                 request.Buf.Bytes(),
				ShouldClose:          false,
			}
			if request.Open == false {
				resp.ShouldClose = true
				ok, err := common.CloseRequest(request.Id)
				if ok != true {
					log.Errorf("%s failed to close request: %v", request.Id.String(), err)
				}
			}
			err := st.Send(resp)
			if err != nil {
				log.Errorf("failed sending message to tunnel stream, exiting", err)
				request.Lock.Unlock()
				closeChan <- true
				return
			}
			request.Buf.Reset()
		}
		request.Lock.Unlock()
	}
}

func receiveData(stream *pb.Tunnel_InitTunnelServer, closeChan chan<- bool) {
	st := *stream
	for {
		message, err := st.Recv()
		if err != nil {
			log.Errorf("failed receiving message from stream, exiting: %v", err)
			closeChan <- true
			return
		}
		reqId, err := uuid.Parse(message.GetRequestId())
		if err != nil {
			log.Errorf(" %s; failed to parse requestId, %v", message.GetRequestId(), err)
		} else {
			request, ok := common.GetRequest(&reqId)
			if ok != true {
				log.Errorf("%s; request not found in openRequests", reqId)
			} else {
				data := message.GetData()
				if len(data) > 0 {
					conn := *request.Conn
					_, err := conn.Write(data)
					if err != nil {
						log.Errorf("%s; failed writing data to socket", reqId)
					}
				}
				if message.ShouldClose == true {
					ok, _ := common.CloseRequest(reqId)
					if ok != true {
						log.Errorf("%s; failed closing request", reqId)
					}
				}
			}
		}
	}
}

