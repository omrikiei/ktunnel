// Package client implements the ktunnel client
package client

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	pb "github.com/omrikiei/ktunnel/api"
	"github.com/omrikiei/ktunnel/pkg/common"
	"github.com/omrikiei/ktunnel/pkg/supervisor"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// ReceiveData serves the cluster-to-local direction of a tunnel until its
// stream ends, and returns the error that ended it. It never returns nil: the
// caller uses its return to decide the tunnel is gone and needs rebuilding, so
// "it stopped, no reason" is not an answer it can act on.
//
// ctx is the caller's context, not the stream's, and is only consulted to tell
// a shutdown apart from a failure.
func ReceiveData(ctx context.Context, conf *Config, st pb.Tunnel_InitTunnelClient, sessionsOut chan<- *common.Session, host string, port int32, scheme string) error {
	for {
		conf.log.Debugf("attempting to receive from stream")
		m, err := st.Recv()
		if err != nil {
			// Recv reports why the stream ended, including our own
			// cancellation, as a Canceled status. The stream context cannot
			// tell the two apart: gRPC cancels it inside Recv before
			// returning, whatever ended the RPC, so a branch on it called
			// every dropped VPN a clean shutdown and logged it at Info as
			// "closing listener" -- the tunnel dying quietly, which is the
			// #114 complaint. Only the caller's context knows whether
			// anybody asked for this.
			if ctx.Err() != nil {
				conf.log.WithError(err).Infof("closing listener on %s:%d", host, port)
				_ = st.CloseSend()
			} else {
				conf.log.WithError(err).Warnf("error reading from stream")
			}
			return err
		}

		// The server reports its own failures on this stream -- a
		// listener it could not bind, for instance -- and those frames
		// carry no session ID, because they are not about a session.
		// Report them; they are the actual diagnosis.
		if m.HasErr {
			logServerError(conf, m, host, port)
			if m.GetRequestID() == "" {
				continue
			}
		}

		requestId, err := uuid.Parse(m.RequestID)
		if err != nil {
			// Without a usable ID there is no session to act on. This
			// used to fall through and operate on the zero UUID, which
			// opened a bogus session and buried whatever the server was
			// actually trying to say.
			conf.log.WithError(err).WithField("session", m.RequestID).Errorf("failed parsing session uuid from stream, skipping")
			continue
		}

		session, exists := conf.sessions.Get(requestId)
		if exists == false {
			conf.log.WithFields(log.Fields{
				"session": m.RequestID,
				"host":    host,
				"port":    port,
			}).Infof("new connection")

			// new session
			conn, err := net.DialTimeout(strings.ToLower(scheme), fmt.Sprintf("%s:%d", host, port), time.Millisecond*500)
			if err != nil {
				conf.log.WithError(err).Errorf("failed connecting to %s on port %d scheme %s", host, port, scheme)
				// close the remote connection
				resp := &pb.SocketDataRequest{
					RequestID:   requestId.String(),
					ShouldClose: true,
				}
				err := st.Send(resp)
				if err != nil {
					conf.log.WithError(err).Errorf("failed sending close message to tunnel stream")
				}

				continue
			}

			session, err = conf.sessions.NewFromStream(requestId, conn)
			if err != nil {
				conf.log.WithError(err).WithField("session", requestId).Errorf("failed tracking new session")
				_ = conn.Close()
				continue
			}
			go ReadFromSession(conf, session, sessionsOut)
		} else if m.ShouldClose {
			session.SetOpen(false)
		}

		// process the data from the server
		handleStreamData(conf, m, session)
	}
}

// logServerError surfaces an error the server reported over the stream. These
// were previously discarded entirely, which is why a server that failed to
// bind its listener showed up on the client as an unexplained UUID parse
// error rather than as the permission or port conflict it actually was.
func logServerError(conf *Config, m *pb.SocketDataResponse, host string, port int32) {
	entry := conf.log.WithFields(log.Fields{"host": host, "port": port})
	if session := m.GetRequestID(); session != "" {
		entry = entry.WithField("session", session)
	}

	msg := m.GetLogMessage()
	if msg == nil {
		entry.Error("tunnel server reported an error but sent no message")
		return
	}

	switch msg.GetLogLevel() {
	case pb.LogLevel_DEBUG, pb.LogLevel_VERBOSE:
		entry.Debugf("tunnel server: %s", msg.GetMessage())
	case pb.LogLevel_INFO:
		entry.Infof("tunnel server: %s", msg.GetMessage())
	case pb.LogLevel_WARNING:
		entry.Warnf("tunnel server: %s", msg.GetMessage())
	default:
		entry.Errorf("tunnel server: %s", msg.GetMessage())
	}
}

func handleStreamData(conf *Config, m *pb.SocketDataResponse, session *common.Session) {
	if !session.IsOpen() {
		conf.log.WithField("session", session.ID).Infof("closed session")
		session.Close()
		return
	}

	data := m.GetData()
	conf.log.WithField("session", session.ID).Debugf("received %d bytes from server", len(data))
	if len(data) > 0 {
		session.Lock()
		conf.log.WithField("session", session.ID).Debugf("wrote %d bytes to conn", len(data))
		_, err := session.Conn.Write(data)
		session.Unlock()
		if err != nil {
			conf.log.WithError(err).WithField("session", session.ID).Errorf("failed writing to socket, closing session")
			session.Close()
			return
		}
	}
}

func ReadFromSession(conf *Config, session *common.Session, sessionsOut chan<- *common.Session) {
	conn := session.Conn
	conf.log.WithField("session", session.ID).Debugf("started reading conn")
	buff := make([]byte, common.BufferSize)

loop:
	for {
		br, err := conn.Read(buff)
		select {
		case <-session.Context.Done():
			return
		default:
			if err != nil {
				if err != io.EOF {
					conf.log.WithError(err).WithField("session", session.ID).Errorf("failed reading from socket")

				} else {
					conf.log.WithField("session", session.ID).Debugf("got EOF from connection")
				}

				session.SetOpen(false)
				sendSession(session, sessionsOut)
				break loop
			}

			conf.log.WithField("session", session.ID).WithError(err).Debugf("read %d bytes from conn", br)

			session.Lock()
			if br > 0 {
				conf.log.WithField("session", session.ID).WithError(err).Debugf("wrote %d bytes to session buf", br)
				_, err = session.Buf.Write(buff[0:br])
			}
			session.Unlock()

			if err != nil {
				conf.log.WithField("session", session.ID).WithError(err).Errorf("failed writing to session buffer")
				break loop
			}
			if !sendSession(session, sessionsOut) {
				break loop
			}
		}

	}
	conf.log.WithField("session", session.ID).Debugf("finished reading session")
}

// sendSession hands a session to the sender, or gives up if the session is
// closed while we wait. It reports whether the send happened.
//
// The bare send this replaces had nobody left to receive it once SendData had
// returned, so every still-open session's reader parked here forever holding
// its net.Conn. One dead tunnel was survivable; with a supervisor rebuilding
// the tunnel it leaked a goroutine and a socket per session per reconnect.
// Closing the session -- which is what tearing a tunnel down does -- now
// releases them.
func sendSession(session *common.Session, sessionsOut chan<- *common.Session) bool {
	select {
	case sessionsOut <- session:
		return true
	case <-session.Context.Done():
		return false
	}
}

// SendData serves the local-to-cluster direction of a tunnel. It returns nil
// when the stream context is done -- a consequence of the stream ending, not a
// reason for it -- and an error when this direction is the one that broke.
func SendData(conf *Config, stream pb.Tunnel_InitTunnelClient, sessions <-chan *common.Session) error {
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case session := <-sessions:
			// read the bytes from the buffer
			// but allow it to keep growing while we send the response
			session.Lock()
			bys := session.Buf.Len()
			bytes := make([]byte, bys)
			_, err := session.Buf.Read(bytes)
			if err != nil {
				session.Unlock()
				conf.log.WithError(err).Errorf("failed reading stream from session %v, exiting", err)
				return errors.Wrap(err, "failed reading from the session buffer")
			}

			conf.log.WithField("session", session.ID).Debugf("read %d from buffer out of %d available", len(bytes), bys)

			resp := &pb.SocketDataRequest{
				RequestID:   session.ID.String(),
				Data:        bytes,
				ShouldClose: !session.Open,
			}
			session.Unlock()

			conf.log.WithFields(log.Fields{
				"session": session.ID,
				"close":   resp.ShouldClose,
			}).Debugf("sending %d bytes to server", len(bytes))
			err = stream.Send(resp)
			if err != nil {
				conf.log.WithError(err).Errorf("failed sending message to tunnel stream, exiting")
				return errors.Wrap(err, "failed sending to the tunnel stream")
			}
			conf.log.WithFields(log.Fields{
				"session": session.ID,
				"close":   resp.ShouldClose,
			}).Debugf("sent %d bytes to server", len(bytes))
		}
	}
}

const (
	// keepaliveTime is how often an otherwise idle connection is pinged.
	// Without this a half-open connection -- a suspended laptop, a dropped
	// VPN -- never errors: the stream goes quiet and the client waits
	// forever for data that is never coming, which is the hang in #114.
	// gRPC raises anything below 10s to 10s.
	//
	// It must also stay above the server's MinTime -- the
	// KeepaliveEnforcementPolicy in pkg/server/server.go, currently 10s. A
	// client that pings more often than the server permits is disconnected
	// with too_many_pings after three of them, so lowering this without
	// lowering that turns the keepalive into the thing that kills the
	// tunnel, roughly every two minutes. That is not hypothetical: it is
	// what a v2.1 client does against a v2.0.x server image, whose default
	// minimum is five minutes.
	keepaliveTime = 30 * time.Second
	// keepaliveTimeout is how long to wait for a ping to be answered before
	// declaring the connection dead.
	keepaliveTimeout = 10 * time.Second
)

// RunClient creates a GRPC tunnel client. It blocks until the tunnels it
// opened stop carrying traffic, and returns the reason they stopped, so a
// caller can rebuild them. Cancelling ctx is a clean shutdown and returns nil.
func RunClient(ctx context.Context, opts ...Option) error {
	conf, err := processArgs(opts)
	if err != nil {
		// Permanent: everything processArgs rejects -- an unusable scheme,
		// no tunnels, no host -- is a configuration error. A supervisor
		// retrying one waits, fails identically, and waits longer, forever
		// by default.
		return supervisor.Permanent(errors.Wrap(err, "failed to parse arguments"))
	}

	// Parse every tunnel before dialling. A malformed port spec is a
	// configuration error no amount of reconnecting will fix, and running
	// the tunnels that did parse would leave the user with a client that
	// reports itself up while silently forwarding nothing.
	tunnels := make([]*common.RedirectRequest, 0, len(conf.tunnels))
	for _, rawTunnelData := range conf.tunnels {
		tunnelData, err := common.ParsePorts(rawTunnelData)
		if err != nil {
			// A typo in a port spec is not a network that will come back.
			return supervisor.Permanent(errors.Wrapf(err, "failed to parse tunnel %q", rawTunnelData))
		}
		tunnels = append(tunnels, tunnelData)
	}

	grpcOpts := []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepaliveTime,
			Timeout:             keepaliveTimeout,
			PermitWithoutStream: true,
		}),
	}
	if conf.TLS {
		creds, err := credentials.NewClientTLSFromFile(conf.certFile, conf.tlsHostOverride)
		if err != nil {
			// Reachable only since --tls started working: an unreadable or
			// malformed --ca-file. The file is not going to appear because
			// we asked again.
			return supervisor.Permanent(errors.Wrap(err, "failed to create TLS credentials"))
		}
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(creds))
	} else {
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", conf.host, conf.port), grpcOpts...)
	if err != nil {
		return errors.Wrap(err, "failed to dial")
	}
	defer conn.Close()

	// Returning means these tunnels are over, and the sessions they opened
	// are sockets to the local service that nothing will read again. The
	// store going out of scope does not close them; this does. A supervisor
	// calls RunClient once per attempt, so anything left here accumulates
	// for as long as the process runs.
	defer conf.sessions.CloseAll()

	client := pb.NewTunnelClient(conn)

	// A tunnel is up when every one of its streams is open -- not when the
	// first is. Reporting on the first would announce a client that is
	// serving some of the ports it was asked for, which is the state
	// RunClient refuses to call working everywhere else.
	pending := int32(len(tunnels))
	opened := func() {
		// Only the transition to zero fires, so established is called at
		// most once per RunClient however many tunnels there are.
		if atomic.AddInt32(&pending, -1) == 0 && conf.established != nil {
			conf.established()
		}
	}

	// Room for every tunnel, so one that fails after we have returned still
	// reports and exits instead of blocking on a channel nobody reads.
	failed := make(chan error, len(tunnels))
	for _, tunnelData := range tunnels {
		go func() {
			failed <- runTunnel(ctx, conf, client, tunnelData, opened)
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-failed:
		if ctx.Err() != nil {
			// We are shutting down; the tunnels are meant to be ending.
			return nil
		}
		// One dead tunnel is reported as the whole client failing. A client
		// serving some of the ports it was asked to serve is not a working
		// client, and the only honest fix is to rebuild all of it.
		return err
	}
}

// runTunnel opens one tunnel's stream and serves it until it stops carrying
// traffic. It calls opened once the stream is up, and always returns non-nil:
// its return is what tells the caller the tunnel is gone, and why.
func runTunnel(ctx context.Context, conf *Config, client pb.TunnelClient, tunnelData *common.RedirectRequest, opened func()) error {
	conf.log.Infof("starting %s tunnel from source %d to target %s:%d", conf.scheme, tunnelData.Source, tunnelData.TargetHost, tunnelData.TargetPort)

	req := &pb.SocketDataRequest{
		Port:     tunnelData.Source,
		LogLevel: 0,
		Scheme:   conf.tunnelScheme,
	}

	// The stream gets a context of its own so that a send half which fails
	// on its own -- leaving a live stream with one dead direction -- can end
	// the stream and wake the receiver, rather than leaving it blocked on a
	// Recv that will never return.
	streamCtx, endStream := context.WithCancel(ctx)
	defer endStream()

	stream, err := client.InitTunnel(streamCtx)
	if err != nil {
		// Setup failures used to be logged and swallowed here, leaving the
		// caller waiting on a tunnel that was never opened.
		return errors.Wrapf(err, "failed opening tunnel for source port %d", tunnelData.Source)
	}
	if err := stream.Send(req); err != nil {
		return errors.Wrapf(err, "failed sending the initial tunnel request for source port %d", tunnelData.Source)
	}

	// The stream is open and the server has been told which port to listen
	// on. That is as far as being "up" can be observed from here: the
	// protocol has no acknowledgement, and a server that cannot bind reports
	// it later as an error frame, which ends the tunnel like any other
	// failure. gRPC does not hand back a stream until the connection is
	// ready, so reaching this line does mean we are talking to a server.
	opened()

	sessions := make(chan *common.Session)
	sendFailed := make(chan error, 1)
	go func() {
		if err := SendData(conf, stream, sessions); err != nil {
			sendFailed <- err
			endStream()
		}
	}()

	// The receiver is the authority on why the tunnel ended: it is the half
	// blocked on Recv, which reports what actually happened to the stream,
	// while the sender mostly observes the aftermath as "context canceled".
	// Waiting for the receiver cannot hang -- anything that ends the stream
	// wakes it, including the send half above.
	reason := ReceiveData(ctx, conf, stream, sessions, tunnelData.TargetHost, tunnelData.TargetPort, conf.scheme)

	// Unless the sender is what ended the stream, in which case the receiver
	// is only reporting the cancellation the sender asked for.
	select {
	case sendErr := <-sendFailed:
		reason = sendErr
	default:
	}

	return errors.Wrapf(reason, "tunnel from source port %d to %s:%d failed", tunnelData.Source, tunnelData.TargetHost, tunnelData.TargetPort)
}

// processArgs processes functional args
func processArgs(opts []Option) (*Config, error) {
	// default arguments
	opt := &Config{
		log: &log.Logger{
			Out: io.Discard,
		},
		scheme: "tcp",
		TLS:    false,
	}

	for _, f := range opts {
		if err := f(opt); err != nil {
			return nil, err
		}
	}

	if len(opt.tunnels) == 0 {
		return nil, fmt.Errorf("no tunnels given")
	}

	if opt.host == "" || opt.port == 0 {
		return nil, fmt.Errorf("missing host configuration")
	}

	// Validated here rather than per tunnel, so an unusable scheme is a
	// configuration error the caller sees immediately. Below, it would have
	// surfaced as a tunnel failure, and a supervisor would retry it forever
	// against something no reconnect can fix.
	//
	// The condition reads inverted and is left as it was found: which
	// schemes are accepted is a behaviour change of its own, separate from
	// where the check lives.
	tunnelScheme, ok := pb.TunnelScheme_value[opt.scheme]
	if ok != false {
		return nil, fmt.Errorf("unsupported connection scheme %s", opt.scheme)
	}
	opt.tunnelScheme = pb.TunnelScheme(tunnelScheme)

	if opt.sessions == nil {
		opt.sessions = common.NewSessionStore()
	}

	return opt, nil
}

// WithServer configures the server this client uses
func WithServer(host string, p int) Option {
	return func(opt *Config) error {
		opt.host = host
		opt.port = p
		return nil
	}
}

// WithTLS configures the tunnel to use TLS
// and sets the certificate expected, and a optional
// tls hostname override.
//
// The flag used to be set from opt.certFile before the assignment below --
// that is, from the certificate of a previous WithTLS on the same config, of
// which there is never one. So opt.TLS stayed false, RunClient took the
// insecure branch, and --tls silently did nothing on every command that has
// ever offered it.
func WithTLS(cert, tlsHostOverride string) Option {
	return func(opt *Config) error {
		opt.TLS = true
		opt.certFile = cert
		opt.tlsHostOverride = tlsHostOverride
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

// WithTunnels configures the tunnels to be exposed
// by this client. Each string should be in the format
// of: localPort:remotePort
func WithTunnels(scheme string, tunnels ...string) Option {
	return func(opt *Config) error {
		opt.scheme = scheme
		opt.tunnels = tunnels
		return nil
	}
}

// Option is an option able to be configured
type Option func(*Config) error

// Config is a config object used to
// configure a GRPC tunnel from the client side.
// ClientOption should be used to modify this
type Config struct {
	host            string
	port            int
	TLS             bool
	certFile        string
	tlsHostOverride string
	scheme          string
	// tunnelScheme is scheme resolved to its protobuf value, by processArgs.
	tunnelScheme pb.TunnelScheme
	log          log.FieldLogger
	tunnels      []string
	sessions     *common.SessionStore
	established  func()
}

// WithEstablishedCallback sets a function to be called once the client's
// tunnels are open, before RunClient blocks serving them.
//
// RunClient does not return while a tunnel is working, so "it is up" cannot be
// reported by returning. A supervisor needs to know: an attempt that never
// reports itself established never resets its backoff, so without this a
// healthy tunnel that flaps once an hour would creep to the maximum retry
// delay and stay there.
//
// The callback runs on the goroutine that opened the last stream, so it should
// not block.
func WithEstablishedCallback(f func()) Option {
	return func(opt *Config) error {
		opt.established = f
		return nil
	}
}

// WithSessionStore sets the store this client tracks its sessions in. If
// unset, the client creates its own.
func WithSessionStore(store *common.SessionStore) Option {
	return func(opt *Config) error {
		opt.sessions = store
		return nil
	}
}
