package server

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"ktunnel/pkg/common"
	pb "ktunnel/tunnel_pb"
	"net"
	"strings"
)

type tunnelServer struct{}

const (
	bufferSize = 4096
)

func NewServer() *tunnelServer {
	return &tunnelServer{}
}

func SendData(stream *pb.Tunnel_InitTunnelServer, sessions <-chan *common.Session, closeChan chan<- bool) {
	for {
		session := <-sessions
		log.Debugf("%s sending %d bytes to client", session.Id, session.Buf.Len())
		session.Lock.Lock()
		st := *stream
		resp := &pb.SocketDataResponse{
			HasErr:      false,
			LogMessage:  nil,
			RequestId:   session.Id.String(),
			Data:        session.Buf.Bytes(),
			ShouldClose: false,
		}
		session.Buf.Reset()
		session.Lock.Unlock()
		log.Debugf("sending %d bytes to client: %s", len(resp.Data), session.Id.String())
		err := st.Send(resp)
		if err != nil {
			log.Errorf("failed sending message to tunnel stream, exiting", err)
			closeChan <- true
			return
		}
		log.Debugf("%s sent to client", session.Id)
		if session.Open == false {
			resp.ShouldClose = true
			ok, err := common.CloseSession(session.Id)
			if ok != true {
				log.Errorf("%s failed to close session: %v", session.Id.String(), err)
			}
		}
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
			session, ok := common.GetSession(reqId)
			if ok != true {
				log.Errorf("%s; session not found in openRequests", reqId)
			} else {
				data := message.GetData()
				if len(data) > 0 {
					conn := session.Conn
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

func readConn(session *common.Session, sessions chan<- *common.Session) {
	log.Debugf("got new session %s", session.Id.String())

	sessions <- session
	// We want to inform the client that we accepted a connection - some weird ass protocols wait for data from the server when connecting
	// Read from socket in a loop and push messages to the sessions channel
	// If the socket is closed, signal the channel to close connection
	buff := make([]byte, bufferSize)
	for {
		br, err := session.Conn.Read(buff)
		log.Debugf("read %d bytes from socket, err: %v", br, err)
		if err != nil {
			session.Open = false
		}
		if br > 0 {
			session.Lock.Lock()
			session.Buf.Write(buff)
			session.Lock.Unlock()
		}
		sessions <- session
		if !session.Open {
			_, _ = common.CloseSession(session.Id)
			return
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
				LogLevel: pb.LogLevel_ERROR,
				Message:  "missing port",
			},
		})
		if err != nil {
			return err
		}
		return errors.New("missing port")
	}
	log.Infof("Opening %s connection on port %d", request.GetScheme(), port)
	ln, err := net.Listen(strings.ToLower(request.GetScheme().String()), fmt.Sprintf(":%d", port))
	if err != nil {
		defer func() {
			log.Errorf("Failed listening on port %d: %v", port, err)
		}()
		_ = stream.Send(&pb.SocketDataResponse{
			HasErr: true,
			LogMessage: &pb.LogMessage{
				LogLevel: pb.LogLevel_ERROR,
				Message:  fmt.Sprintf("failed opening listener type %s on port %d: %v", request.GetScheme(), request.GetPort(), err),
			},
		})
		return err
	}

	sessions := make(chan *common.Session)
	closeChan := make(chan bool, 1)
	go func(close <-chan bool) {
		<-close
		log.Infof("Closing connection on port %d", port)
		_ = ln.Close()
	}(closeChan)

	go ReceiveData(&stream, closeChan)
	go SendData(&stream, sessions, closeChan)

	for {
		connection, err := ln.Accept()
		log.Debugf("Accepted new connection %v; %v", connection, err)
		if err != nil {
			return err
		}
		// socket -> stream
		session := common.NewSession(connection)
		go readConn(session, sessions)
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
