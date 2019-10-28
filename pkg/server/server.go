package server

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io"
	"ktunnel/pkg/common"
	pb "ktunnel/tunnel_pb"
	"net"
	"strings"
)

const (
	bufferSize = 32*1024
)

type tunnelServer struct {}

func NewServer() *tunnelServer {
	return &tunnelServer{}
}

func SendData(stream *pb.Tunnel_InitTunnelServer, requests <-chan *common.Request, closeChan chan<- bool) {
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

func ReceiveData(stream *pb.Tunnel_InitTunnelServer, closeChan chan<- bool) {
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

func (t *tunnelServer) InitTunnel(stream pb.Tunnel_InitTunnelServer) error {
	request, err := stream.Recv()
	if err != nil {
		log.Fatalf("Failed receiving initial connection from tunnel")
	}
	port := request.GetPort()
	if port == 0 {
		err := stream.Send(&pb.SocketDataResponse{
			HasErr: true,
			LogMessage: &pb.LogMessage{
				LogLevel:             pb.LogLevel_ERROR,
				Message:              "missing port",
			},
		})
		if err != nil {
			return err
		}
		return errors.New("missing port")
	}
	log.Infof("Opening %s conenction on port %d", request.GetScheme(), port)
	ln, err := net.Listen(strings.ToLower(request.GetScheme().String()), fmt.Sprintf(":%d", port))
	if err != nil {
		defer func() {
			log.Errorf("Failed listening on port %d: %v", port, err)
		}()
		_ = stream.Send(&pb.SocketDataResponse{
			HasErr: true,
			LogMessage: &pb.LogMessage{
				LogLevel:             pb.LogLevel_ERROR,
				Message:              fmt.Sprintf("failed opening listener type %s on port %d: %v", request.GetScheme(), request.GetPort(), err),
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

	go ReceiveData(&stream, closeChan)
	go SendData(&stream, requests, closeChan)

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

func RunServer(port *int, tls *bool, keyFile, certFile *string) error {
	log.Infof("Starting to listen on port %d", *port)
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	var opts []grpc.ServerOption
	if *tls {
		creds, err := credentials.NewServerTLSFromFile(*certFile, *keyFile)
		if err != nil {
			log.Fatalf("Failed to generate credentials %v", err)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	}
	grpcServer := grpc.NewServer(opts...)
	pb.RegisterTunnelServer(grpcServer, NewServer())
	err = grpcServer.Serve(lis)
	return err
}