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

func SendData(stream *pb.Tunnel_InitTunnelServer, sessions <-chan *common.Session, closeChan chan<- bool) {
	for {
		session := <-sessions
		session.Lock.Lock()
		if session.Buf.Len() > 0 || session.Open == false {
			st := *stream
			resp := &pb.SocketDataResponse{
				HasErr:      false,
				LogMessage:  nil,
				RequestId:   session.Id.String(),
				Data:        session.Buf.Bytes(),
				ShouldClose: false,
			}
			if session.Open == false {
				resp.ShouldClose = true
				ok, err := common.CloseSession(session.Id)
				if ok != true {
					log.Errorf("%s failed to close session: %v", session.Id.String(), err)
				}
			}
			err := st.Send(resp)
			if err != nil {
				log.Errorf("failed sending message to tunnel stream, exiting", err)
				session.Lock.Unlock()
				closeChan <- true
				return
			}
			session.Buf.Reset()
		}
		session.Lock.Unlock()
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
			session, ok := common.GetSession(&reqId)
			if ok != true {
				log.Errorf("%s; session not found in openRequests", reqId)
			} else {
				data := message.GetData()
				if len(data) > 0 {
					conn := *session.Conn
					_, err := conn.Write(data)
					if err != nil {
						log.Errorf("%s; failed writing data to socket", reqId)
					}
				}
				if message.ShouldClose == true {
					ok, _ := common.CloseSession(reqId)
					if ok != true {
						log.Errorf("%s; failed closing session", reqId)
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

	sessions := make(chan *common.Session)
	closeChan := make(chan bool, 1)
	go func(close <-chan bool){
		<-close
		log.Infof("Closing connection on port %d", port)
		_ = ln.Close()
	}(closeChan)

	go ReceiveData(&stream, closeChan)
	go SendData(&stream, sessions, closeChan)

	for {
		connection, err := ln.Accept()
		if err != nil {
			return err
		}
		// socket -> stream
		go func(connection net.Conn) {
			if err != nil {
				log.Errorf("Failed accepting connection on port %d", port)
				return
			}
			request := common.NewSession(&connection)
			log.Debugf("got new request %s", request.Id.String())
			// Read from socket in a loop and push messages to the sessions channel
			// If the socket is closed, signal the channel to close connection

			for {
				buff := make([]byte, bufferSize)
				br, err := connection.Read(buff)
				request.Lock.Lock()
				if err != nil {
					if err == io.EOF {
						continue
					}
					log.Errorf("%s; failed to read from server socket: %v", request.Id.String(), err)
					request.Open = false
					sessions <- request
					request.Lock.Unlock()
					break
				} else {
					_, err := request.Buf.Write(buff[:br])
					if err != nil {
						log.Errorf("%s; failed to write to request buffer: %v", request.Id.String(), err)
					}
				}
				request.Lock.Unlock()
				sessions <- request
			}
		}(connection)
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