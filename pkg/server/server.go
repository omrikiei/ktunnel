package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/pkg/errors"

	"github.com/google/uuid"
	"github.com/omrikiei/ktunnel/pkg/common"
	pb "github.com/omrikiei/ktunnel/tunnel_pb"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type tunnelServer struct {
	conf *ServerConfig
}

// NewServer creates a new GRPC handler instance that
// can be attached to a GRPC server
func NewServer(conf *ServerConfig) *tunnelServer {
	return &tunnelServer{conf}
}

// SendData handles data coming from our TCP listener, via the sessions channel, and
// republishes it over GRPC
func SendData(conf *ServerConfig, stream pb.Tunnel_InitTunnelServer, sessions <-chan *common.Session, closeChan chan<- bool) {
	for {
		session := <-sessions

		// read the bytes from the buffer
		// but allow it to keep growing while we send the response
		session.Lock()
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
		session.Unlock()

		conf.log.WithFields(log.Fields{
			"session": session.Id,
			"close":   resp.ShouldClose,
		}).Debugf("sending %d bytes to client", len(bytes))
		err := stream.Send(resp)
		if err != nil {
			conf.log.WithError(err).Errorf("failed sending message to tunnel stream")
			continue
		}
		conf.log.WithFields(log.Fields{
			"session": session.Id,
			"close":   resp.ShouldClose,
		}).Debugf("sent %d bytes to client", len(bytes))
	}
}

func ReceiveData(conf *ServerConfig, stream pb.Tunnel_InitTunnelServer, closeChan chan<- bool) {
	for {
		message, err := stream.Recv()
		if err != nil {
			conf.log.WithError(err).Warnf("failed receiving message from stream")
			continue
		}

		reqId, err := uuid.Parse(message.GetRequestId())
		if err != nil {
			conf.log.WithError(err).WithField("session", message.GetRequestId()).Errorf("failed to parse requestId")
			continue
		}

		session, ok := common.GetSession(reqId)
		if ok != true {
			conf.log.WithField("session", reqId).Errorf("session not found in openRequests")
			continue
		}

		data := message.GetData()
		br := len(data)

		conf.log.WithFields(log.Fields{
			"session": session.Id,
			"close":   message.ShouldClose,
		}).Debugf("received %d bytes from client", len(data))

		// send data if we received any
		if br > 0 {
			conf.log.WithField("session", reqId).Debugf("writing %d bytes to conn", br)
			_, err := session.Conn.Write(data)
			if err != nil {
				conf.log.WithError(err).WithField("session", reqId).Errorf("failed writing data to socket")
				message.ShouldClose = true
			} else {
				conf.log.WithField("session", reqId).Debugf("wrote %d bytes to conn", br)
			}
		}

		if message.ShouldClose == true {
			conf.log.WithField("session", reqId).Debug("closing session")
			session.Close()
			conf.log.WithField("session", reqId).Debug("closed session")
		}
	}
}

func readConn(conf *ServerConfig, session *common.Session, sessions chan<- *common.Session) {
	conf.log.WithField("session", session.Id.String()).Info("new conn connection")

	for {
		buff := make([]byte, common.BufferSize)
		br, err := session.Conn.Read(buff)
		conf.log.WithError(err).Infof("read %d bytes from conn", br)

		session.Lock()
		if err != nil {
			if err == io.EOF {
				return
			}
			conf.log.WithError(err).WithField("session", session.Id).Infof("failed to read from conn")

			// setting Open to false triggers SendData() to
			// send ShouldClose
			session.Open = false
		}

		// write the data to the session buffer, if we have data
		if br > 0 {
			session.Buf.Write(buff[0:br])
		}
		session.Unlock()

		sessions <- session
		if session.Open == false {
			return
		}
	}
}

func (t *tunnelServer) InitTunnel(stream pb.Tunnel_InitTunnelServer) error {
	request, err := stream.Recv()
	if err != nil {
		return errors.Wrap(err, "failed to read handshake")
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

		return fmt.Errorf("missing port")
	}

	t.conf.log.WithFields(log.Fields{
		"port":   port,
		"schema": request.GetScheme(),
	}).Infof("opening connection")
	ln, err := net.Listen(strings.ToLower(request.GetScheme().String()), fmt.Sprintf(":%d", port))
	if err != nil {
		defer func() {
			t.conf.log.WithError(err).Errorf("Failed listening on port %d", port)
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
		t.conf.log.WithField("port", port).Infof("closing connection")
		_ = ln.Close()
	}(closeChan)

	go func() {
		ReceiveData(t.conf, stream, closeChan)
		t.conf.log.WithField("port", port).Debug("client receiver died (client -> conn)")
	}()
	go func() {
		SendData(t.conf, stream, sessions, closeChan)
		t.conf.log.WithField("port", port).Debug("conn receiver died (conn -> client)")
	}()

	for {
		connection, err := ln.Accept()
		t.conf.log.WithError(err).Debugf("Accepted new connection %v", connection)
		if err != nil {
			return err
		}

		// socket -> stream
		session := common.NewSession(connection)
		go readConn(t.conf, session, sessions)
	}
}

// RunServer creates a GRPC tunnel
func RunServer(ctx context.Context, opts ...ServerOption) error {
	conf, err := processArgs(opts)
	if err != nil {
		return errors.Wrap(err, "failed to parse arguments")
	}

	var grpcOpts []grpc.ServerOption
	if conf.TLS {
		creds, err := credentials.NewServerTLSFromFile(conf.certFile, conf.keyFile)
		if err != nil {
			conf.log.Fatalf("Failed to generate credentials %v", err)
		}
		grpcOpts = []grpc.ServerOption{grpc.Creds(creds)}
	}

	conf.log.Infof("Starting to listen on port %d", conf.port)
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", conf.port))
	if err != nil {
		return errors.Wrap(err, "failed to start GRPC listener")
	}

	// handle context cancellation, shut down the server
	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()

	grpcServer := grpc.NewServer(grpcOpts...)
	pb.RegisterTunnelServer(grpcServer, NewServer(conf))
	return grpcServer.Serve(lis)
}

// processArgs processes functional args
func processArgs(opts []ServerOption) (*ServerConfig, error) {
	// default arguments
	opt := &ServerConfig{
		port: 5000,
		log: &log.Logger{
			Out: os.Stdout,
			Level: log.InfoLevel,
			Formatter: &log.TextFormatter{},
		},
		TLS: false,
	}

	for _, f := range opts {
		if err := f(opt); err != nil {
			return nil, err
		}
	}

	return opt, nil
}

// WithPort configures the GRPC tunnel server
// to listen on a given port.
func WithPort(p int) ServerOption {
	return func(opt *ServerConfig) error {
		opt.port = p
		return nil
	}
}

// WithTLS configures the GRPC tunnel server
// to use TLS
func WithTLS(cert, key string) ServerOption {
	return func(opt *ServerConfig) error {
		opt.TLS = true
		opt.certFile = cert
		opt.keyFile = key
		return nil
	}
}

// WithLogger sets the logger to be used by the server.
// if not set, output will be discarded
func WithLogger(l log.FieldLogger) ServerOption {
	return func(opt *ServerConfig) error {
		opt.log = l
		return nil
	}
}

// ServerOption is an option able to be configured
type ServerOption func(*ServerConfig) error

// ServerConfig is a config object used to
// configure a GRPC Server. ServerOption should
// be used to modify this
type ServerConfig struct {
	port     int
	TLS      bool
	keyFile  string
	log      log.FieldLogger
	certFile string
}
