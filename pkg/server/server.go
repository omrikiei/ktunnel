// Package server provides a GRPC server that can be used to tunnel TCP connections
package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pkg/errors"

	pb "github.com/omrikiei/ktunnel/api"
	"github.com/omrikiei/ktunnel/pkg/common"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

type TunnelServer struct {
	conf *Config
}

// NewServer creates a new GRPC handler instance that
// can be attached to a GRPC server
func NewServer(conf *Config) *TunnelServer {
	return &TunnelServer{conf}
}

// SendData handles data coming from our TCP listener, via the sessions channel, and
// republishes it over GRPC
func SendData(conf *Config, stream pb.Tunnel_InitTunnelServer, sessions <-chan *common.Session) {
	for {
		select {
		case <-stream.Context().Done():
			return
		case session := <-sessions:
			// read the bytes from the buffer
			// but allow it to keep growing while we send the response
			session.Lock()
			bys := session.Buf.Len()
			bytes := make([]byte, bys)
			_, err := session.Buf.Read(bytes)
			resp := &pb.SocketDataResponse{
				HasErr:      err != nil,
				LogMessage:  nil,
				Data:        bytes,
				RequestID:   session.ID.String(),
				ShouldClose: !session.Open,
			}
			session.Unlock()

			conf.log.WithFields(log.Fields{
				"session": session.ID,
				"close":   resp.ShouldClose,
			}).Debugf("sending %d bytes to client", len(bytes))
			err = stream.Send(resp)
			if err != nil {
				conf.log.WithError(err).Errorf("failed sending message to tunnel stream")
				continue
			}
			conf.log.WithFields(log.Fields{
				"session": session.ID,
				"close":   resp.ShouldClose,
			}).Debugf("sent %d bytes to client", len(bytes))
		}
	}
}

func ReceiveData(conf *Config, stream pb.Tunnel_InitTunnelServer) {
	for {
		select {
		case <-stream.Context().Done():
			return
		default:
			message, err := stream.Recv()
			if err != nil {
				conf.log.WithError(err).Warnf("failed receiving message from stream")
				continue
			}

			reqID, err := uuid.Parse(message.GetRequestID())
			if err != nil {
				conf.log.WithError(err).WithField("session", message.GetRequestID()).Errorf("failed to parse requestId")
				continue
			}

			session, ok := conf.sessions.Get(reqID)
			if !ok {
				// A close frame for a session we have already reaped is
				// expected -- sessions linger only briefly after closing --
				// and there is nothing left to close. Anything else is a
				// genuine mismatch. Either way there is no session to
				// dereference below, so both must stop here.
				if message.ShouldClose {
					conf.log.WithField("session", reqID).Debug("close for an already-closed session, ignoring")
				} else {
					conf.log.WithField("session", reqID).Errorf("session not found in openRequests")
				}
				continue
			}

			data := message.GetData()
			br := len(data)

			conf.log.WithFields(log.Fields{
				"session": session.ID,
				"close":   message.ShouldClose,
			}).Debugf("received %d bytes from client", len(data))

			// send data if we received any
			if br > 0 && session.IsOpen() {
				conf.log.WithField("session", reqID).Debugf("writing %d bytes to conn", br)
				_, err := session.Conn.Write(data)
				if err != nil {
					conf.log.WithError(err).WithField("session", reqID).Errorf("failed writing data to socket")
					message.ShouldClose = true
				} else {
					conf.log.WithField("session", reqID).Debugf("wrote %d bytes to conn", br)
				}
			}

			if message.ShouldClose {
				conf.log.WithField("session", reqID).Debug("closing session")
				session.Close()
				conf.log.WithField("session", reqID).Debug("closed session")
			}
		}

	}
}

// sendSession hands a session to SendData, or gives up if the tunnel it
// belongs to ends while it waits. It reports whether the send happened.
//
// The bare send this replaces had nobody left to receive it once SendData had
// returned with its stream, so the reader of every still-open connection
// parked here forever holding its socket. That was survivable when a dead
// tunnel took the process with it. It is not now: a server pod stays up for
// days and sees a stream die every time a client's network blinks, leaking a
// goroutine and a socket per open session each time.
//
// Giving up closes the session, because on this side nothing else will. The
// client closes its store when RunClient returns; the server has no such
// moment -- it serves many streams over its lifetime -- so the connection
// would otherwise stay open with nothing at either end of it.
func sendSession(ctx context.Context, session *common.Session, sessions chan<- *common.Session) bool {
	select {
	case sessions <- session:
		return true
	case <-ctx.Done():
	case <-session.Context.Done():
	}

	session.Close()
	return false
}

func readConn(ctx context.Context, conf *Config, session *common.Session, sessions chan<- *common.Session) {
	conf.log.WithField("session", session.ID.String()).Info("new connection")
	if !sendSession(ctx, session, sessions) {
		return
	}

	for {

		buff := make([]byte, common.BufferSize)
		br, err := session.Conn.Read(buff)

		select {
		case <-ctx.Done():
			conf.log.Info("closing connection")
			session.Close()
			return
		default:
			conf.log.WithError(err).Debugf("read %d bytes from conn", br)

			session.Lock()
			if err != nil {
				if err != io.EOF {
					conf.log.WithError(err).WithField("session", session.ID).Infof("failed to read from conn")
				}

				// setting Open to false triggers SendData() to
				// send ShouldClose
				session.Open = false
			}

			// write the data to the session buffer, if we have data
			if br > 0 {
				session.Buf.Write(buff[0:br])
			}
			session.Unlock()

			if !sendSession(ctx, session, sessions) {
				return
			}
			if !session.IsOpen() {
				return
			}
		}
	}
}

func (t *TunnelServer) InitTunnel(stream pb.Tunnel_InitTunnelServer) error {
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
	go func() {
		<-stream.Context().Done()
		t.conf.log.WithField("port", port).Infof("tunnel closed by client, closing connections")
		_ = ln.Close()
	}()

	go func() {
		ReceiveData(t.conf, stream)
		t.conf.log.WithField("port", port).Debug("client receiver died (client -> conn)")
	}()
	go func() {
		SendData(t.conf, stream, sessions)
		t.conf.log.WithField("port", port).Debug("conn receiver died (conn -> client)")
	}()

	for {
		connection, err := ln.Accept()
		t.conf.log.WithError(err).Debugf("Accepted new connection %v", connection)
		if err != nil {
			return err
		}

		// socket -> stream
		session := t.conf.sessions.New(connection)
		go readConn(stream.Context(), t.conf, session, sessions)
	}
}

// RunServer creates a GRPC tunnel
func RunServer(ctx context.Context, opts ...Option) error {
	conf, err := processArgs(opts)
	if err != nil {
		return errors.Wrap(err, "failed to parse arguments")
	}

	// Clients keep the tunnel's connection alive with pings, and a gRPC
	// server rejects pings closer together than five minutes by default:
	// after three of them it sends GOAWAY "too_many_pings" and drops the
	// connection. The server would then be tearing down the very tunnel the
	// pings exist to protect, roughly every two minutes. MinTime is gRPC's
	// own client-side floor, so any conformant client is permitted.
	grpcOpts := []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		// Without this, Stop returns once the connections are closed and
		// leaves the method handlers running. Here the handler *is* the
		// tunnel: InitTunnel owns the listener on the tunnelled port and
		// closes it on its way out, so a Stop that does not wait for it
		// reports the server down while the port is still bound. Whoever
		// binds it next -- a reconnecting client, a restarted pod -- loses
		// that race, intermittently and for no visible reason.
		//
		// The wait is bounded by the handlers themselves: closing the
		// transports cancels every stream, and each InitTunnel closes its
		// listener when its stream is cancelled, which is what ends its
		// accept loop.
		grpc.WaitForHandlers(true),
	}
	if conf.TLS {
		creds, err := credentials.NewServerTLSFromFile(conf.certFile, conf.keyFile)
		if err != nil {
			conf.log.Fatalf("Failed to generate credentials %v", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	}

	conf.log.Infof("Starting to listen on port %d", conf.port)
	lis, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", conf.port))
	if err != nil {
		return errors.Wrap(err, "failed to start GRPC listener")
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	pb.RegisterTunnelServer(grpcServer, NewServer(conf))

	// Cancelling the context has to stop the server, not just its listener.
	// Closing lis only stops new connections being accepted: every stream
	// already open keeps running, and each of those streams is an
	// InitTunnel holding a listener of its own on a tunnelled port. A
	// "stopped" server therefore went on serving traffic and holding ports,
	// so nothing could take its place -- a caller that restarts a server on
	// the same ports got a bind failure, or worse, silently kept talking to
	// the old one.
	//
	// Stop, not GracefulStop: GracefulStop stops accepting and then blocks
	// until every open RPC has finished. Every RPC here is a tunnel that
	// ends only when its client goes away, so on the shutdown path that
	// matters -- the client is still connected -- GracefulStop would never
	// return, and Ctrl+C would hang. Stop closes the transports, which
	// cancels the streams, which is what makes each InitTunnel release its
	// tunnelled port.
	stopped := make(chan struct{})
	watch, endWatch := context.WithCancel(ctx)
	defer endWatch()
	go func() {
		defer close(stopped)
		<-watch.Done()
		grpcServer.Stop()
	}()

	err = grpcServer.Serve(lis)

	// Serve has returned, so nothing is left to watch for. If the caller
	// never cancelled -- Serve failed on its own -- this releases the
	// watcher and stops the server for tidiness; if the caller did, Stop is
	// already running and this changes nothing.
	endWatch()

	// Waiting for Stop is the point of all this: it is what closes the
	// tunnelled listeners, and it returns only once the InitTunnel handlers
	// holding them have. A RunServer that returned before that would report
	// the server down while its ports were still bound, which is precisely
	// the race a restart on the same port loses.
	<-stopped

	if ctx.Err() != nil {
		// A shutdown the caller asked for. Serve reports it as a closed
		// listener or ErrServerStopped, neither of which is a failure the
		// caller wants to see -- `ktunnel server` used to log.Fatal on
		// Ctrl+C because of it.
		return nil
	}
	return err
}

// processArgs processes functional args
func processArgs(opts []Option) (*Config, error) {
	// default arguments
	opt := &Config{
		port: 5000,
		log: &log.Logger{
			Out: io.Discard,
		},
		TLS: false,
	}

	for _, f := range opts {
		if err := f(opt); err != nil {
			return nil, err
		}
	}

	if opt.sessions == nil {
		opt.sessions = common.NewSessionStore()
	}

	return opt, nil
}

// WithPort configures the GRPC tunnel server
// to listen on a given port.
func WithPort(p int) Option {
	return func(opt *Config) error {
		opt.port = p
		return nil
	}
}

// WithTLS configures the GRPC tunnel server
// to use TLS
func WithTLS(cert, key string) Option {
	return func(opt *Config) error {
		opt.TLS = true
		opt.certFile = cert
		opt.keyFile = key
		return nil
	}
}

// WithLogger sets the logger to be used by the server.
// if not set, output will be discarded
func WithLogger(l log.FieldLogger) Option {
	return func(opt *Config) error {
		opt.log = l
		return nil
	}
}

// Option is an option able to be configured
type Option func(*Config) error

// Config is a config object used to
// configure a GRPC Server. ServerOption should
// be used to modify this
type Config struct {
	port     int
	TLS      bool
	keyFile  string
	log      log.FieldLogger
	certFile string
	sessions *common.SessionStore
}

// WithSessionStore sets the store this server tracks its sessions in. If
// unset, the server creates its own.
func WithSessionStore(store *common.SessionStore) Option {
	return func(opt *Config) error {
		opt.sessions = store
		return nil
	}
}
