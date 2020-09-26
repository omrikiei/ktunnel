package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/omrikiei/ktunnel/pkg/common"
	pb "github.com/omrikiei/ktunnel/tunnel_pb"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type tunnelServer struct{}

func NewServer() *tunnelServer {
	return &tunnelServer{}
}

func SendData(stream pb.Tunnel_InitTunnelServer, sessions <-chan *common.Session, closeChan chan<- bool) {
	for {
		session := <-sessions

		// read the bytes from the buffer
		// but allow it to keep growing while we send the response
		session.Lock.Lock()
		bys := session.Buf.Len()
		bytes := make([]byte, bys)
		session.Buf.Read(bytes)

		resp := &pb.SocketDataResponse{
			HasErr:      false,
			LogMessage:  nil,
			Data:        bytes,
			RequestId:   session.Id.String(),
			ShouldClose: !session.Open,
		}
		session.Lock.Unlock()

		log.WithFields(log.Fields{
			"session": session.Id,
			"close":   resp.ShouldClose,
		}).Infof("sending %d bytes to client", len(bytes))
		err := stream.Send(resp)
		if err != nil {
			log.WithError(err).Errorf("failed sending message to tunnel stream, exiting")
			return
		}
		log.WithFields(log.Fields{
			"session": session.Id,
			"close":   resp.ShouldClose,
		}).Infof("sent %d bytes to client", len(bytes))
	}
}

func ReceiveData(stream pb.Tunnel_InitTunnelServer, closeChan chan<- bool) {
	for {
		message, err := stream.Recv()
		if err != nil {
			log.WithError(err).Warnf("failed receiving message from stream")
			continue
		}

		reqId, err := uuid.Parse(message.GetRequestId())
		if err != nil {
			log.WithError(err).WithField("session", message.GetRequestId()).Errorf("failed to parse requestId")
			continue
		}

		session, ok := common.GetSession(reqId)
		if ok != true {
			log.WithField("session", reqId).Errorf("session not found in openRequests")
			continue
		}

		data := message.GetData()
		br := len(data)

		log.WithFields(log.Fields{
			"session": session.Id,
			"close":   message.ShouldClose,
		}).Infof("received %d bytes from client", len(data))

		// send data if we received any
		if br > 0 {
			log.WithField("session", reqId).Infof("writing %d bytes to conn", br)
			_, err := session.Conn.Write(data)
			if err != nil {
				log.WithError(err).WithField("session", reqId).Errorf("failed writing data to socket")
				message.ShouldClose = true
			} else {
				log.WithField("session", reqId).Infof("wrote %d bytes to conn", br)
			}
		}

		if message.ShouldClose == true {
			log.WithField("session", reqId).Infof("closing session")
			session.Close()
			log.WithField("session", reqId).Infof("closed session")
		}
	}
}

func readConn(session *common.Session, sessions chan<- *common.Session) {
	log.WithField("session", session.Id.String()).Info("new conn connection")

	for {
		buff := make([]byte, common.BufferSize)
		br, err := session.Conn.Read(buff)
		log.WithError(err).Infof("read %d bytes from conn", br)

		session.Lock.Lock()
		if err != nil {
			if err == io.EOF {
				return
			}
			log.WithError(err).WithField("session", session.Id).Infof("failed to read from conn")

			// setting Open to false triggers SendData() to
			// send ShouldClose
			session.Open = false
		}

		// write the data to the session buffer, if we have data
		if br > 0 {
			session.Buf.Write(buff[0:br])
		}
		session.Lock.Unlock()

		sessions <- session
		if session.Open == false {
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

	log.WithFields(log.Fields{
		"port":   port,
		"schema": request.GetScheme(),
	}).Infof("opening connection")
	ln, err := net.Listen(strings.ToLower(request.GetScheme().String()), fmt.Sprintf(":%d", port))
	if err != nil {
		defer func() {
			log.WithError(err).Errorf("Failed listening on port %d", port)
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
		log.WithField("port", port).Infof("closing connection")
		_ = ln.Close()
	}(closeChan)

	go func() {
		ReceiveData(stream, closeChan)
		log.WithField("port", port).Warnf("client receiver died (client -> conn)")
	}()
	go func() {
		SendData(stream, sessions, closeChan)
		log.WithField("port", port).Warnf("conn receiver died (conn -> client)")
	}()

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

func RunServer(ctx context.Context, port *int, tls *bool, keyFile, certFile *string) error {
	log.Infof("Starting to listen on port %d", *port)
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// handle context cancellation, shut down the server
	go func() {
		<-ctx.Done()
		lis.Close()
	}()

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
	return grpcServer.Serve(lis)
}
