package client

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/omrikiei/ktunnel/pkg/common"
	pb "github.com/omrikiei/ktunnel/tunnel_pb"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type Message struct {
	c *net.Conn
	d *[]byte
}

func ReceiveData(conf *ClientConfig, st pb.Tunnel_InitTunnelClient, sessionsOut chan<- *common.Session, port int32, scheme string) {
loop:
	for {
		conf.log.Debugf("attempting to receive from stream")
		m, err := st.Recv()
		select {
		case <-st.Context().Done():
			conf.log.WithError(st.Context().Err()).Infof("closing listener on %d", port)
			_ = st.CloseSend()
			break loop
		default:
			if err != nil {
				conf.log.WithError(err).Warnf("error reading from stream")
				break loop
			}

			requestId, err := uuid.Parse(m.RequestId)
			if err != nil {
				conf.log.WithError(err).WithField("session", m.RequestId).Errorf("failed parsing session uuid from stream, skipping")
			}

			session, exists := common.GetSession(requestId)
			if exists == false {
				conf.log.WithFields(log.Fields{
					"session": m.RequestId,
					"port":    port,
				}).Infof("new connection")

				// new session
				conn, err := net.DialTimeout(strings.ToLower(scheme), fmt.Sprintf("localhost:%d", port), time.Millisecond*500)
				if err != nil {
					conf.log.WithError(err).Errorf("failed connecting to localhost on port %d scheme %s", port, scheme)
					// close the remote connection
					resp := &pb.SocketDataRequest{
						RequestId:   requestId.String(),
						ShouldClose: true,
					}
					err := st.Send(resp)
					if err != nil {
						conf.log.WithError(err).Errorf("failed sending close message to tunnel stream")
					}

					continue
				} else {
					session = common.NewSessionFromStream(requestId, conn)
					go ReadFromSession(conf, session, sessionsOut)
				}
			} else if m.ShouldClose {
				session.Open = false
			}

			// process the data from the server
			handleStreamData(conf, m, session)
		}
	}
}

func handleStreamData(conf *ClientConfig, m *pb.SocketDataResponse, session *common.Session) {
	if session.Open == false {
		conf.log.WithField("session", session.Id).Infof("closed session")
		session.Close()
		return
	}

	data := m.GetData()
	conf.log.WithField("session", session.Id).Debugf("received %d bytes from server", len(data))
	if len(data) > 0 {
		session.Lock()
		conf.log.WithField("session", session.Id).Debugf("wrote %d bytes to conn", len(data))
		_, err := session.Conn.Write(data)
		session.Unlock()
		if err != nil {
			conf.log.WithError(err).WithField("session", session.Id).Errorf("failed writing to socket, closing session")
			session.Close()
			return
		}
	}
}

func ReadFromSession(conf *ClientConfig, session *common.Session, sessionsOut chan<- *common.Session) {
	conn := session.Conn
	conf.log.WithField("session", session.Id).Debugf("started reading conn")
	buff := make([]byte, common.BufferSize)

loop:
	for {
		br, err := conn.Read(buff)
		select {
		case <-session.Context.Done():
			return
		default:
			if err != nil {
				if err == io.EOF {
					break loop
				}

				conf.log.WithError(err).WithField("session", session.Id).Errorf("failed reading from socket")
				session.Open = false
				sessionsOut <- session
				break loop
			}

			conf.log.WithField("session", session.Id).WithError(err).Debugf("read %d bytes from conn", br)

			session.Lock()
			if br > 0 {
				conf.log.WithField("session", session.Id).WithError(err).Debugf("wrote %d bytes to session buf", br)
				_, err = session.Buf.Write(buff[0:br])
			}
			session.Unlock()

			if err != nil {
				conf.log.WithField("session", session.Id).WithError(err).Errorf("failed writing to session buffer")
				break loop
			}
			sessionsOut <- session
		}

	}
	conf.log.WithField("session", session.Id).Debugf("finished reading session")
}

func SendData(conf *ClientConfig, stream pb.Tunnel_InitTunnelClient, sessions <-chan *common.Session) {
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
			if err != nil {
				conf.log.WithError(err).Errorf("failed reading stream from session %v, exiting", err)
				return
			}

			conf.log.WithField("session", session.Id).Debugf("read %d from buffer out of %d available", len(bytes), bys)

			resp := &pb.SocketDataRequest{
				RequestId:   session.Id.String(),
				Data:        bytes,
				ShouldClose: !session.Open,
			}
			session.Unlock()

			conf.log.WithFields(log.Fields{
				"session": session.Id,
				"close":   resp.ShouldClose,
			}).Debugf("sending %d bytes to server", len(bytes))
			err = stream.Send(resp)
			if err != nil {
				conf.log.WithError(err).Errorf("failed sending message to tunnel stream, exiting")
				return
			}
			conf.log.WithFields(log.Fields{
				"session": session.Id,
				"close":   resp.ShouldClose,
			}).Debugf("sent %d bytes to server", len(bytes))
		}
	}
}

// RunClient creates a GRPC tunnel client
func RunClient(ctx context.Context, opts ...ClientOption) error {
	conf, err := processArgs(opts)
	if err != nil {
		return errors.Wrap(err, "failed to parse arguments")
	}

	var grpcOpts []grpc.DialOption
	if conf.TLS {
		creds, err := credentials.NewClientTLSFromFile(conf.certFile, conf.tlsHostOverride)
		if err != nil {
			return errors.Wrap(err, "failed to create TLS credentials")
		}
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(creds))
	} else {
		grpcOpts = append(grpcOpts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", conf.host, conf.port), grpcOpts...)
	if err != nil {
		return errors.Wrap(err, "failed to dial")
	}
	defer conn.Close()

	client := pb.NewTunnelClient(conn)
	for _, rawTunnelData := range conf.tunnels {
		tunnelData, err := common.ParsePorts(rawTunnelData)
		if err != nil {
			conf.log.Error(err)
			continue
		}

		go func() {
			conf.log.Infof("starting %s tunnel from source %d to target %d", conf.scheme, tunnelData.Source, tunnelData.Target)
			tunnelScheme, ok := pb.TunnelScheme_value[conf.scheme]
			if ok != false {
				conf.log.Fatalf("unsupported connection scheme %s", conf.scheme)
			}

			req := &pb.SocketDataRequest{
				Port:     tunnelData.Source,
				LogLevel: 0,
				Scheme:   pb.TunnelScheme(tunnelScheme),
			}

			stream, err := client.InitTunnel(ctx)
			if err != nil {
				conf.log.Errorf("Error sending init tunnel request: %v", err)
			} else {
				err := stream.Send(req)
				if err != nil {
					conf.log.WithError(err).Errorf("Failed to send initial tunnel request to server")
					return
				}

				sessions := make(chan *common.Session)
				go ReceiveData(conf, stream, sessions, tunnelData.Target, conf.scheme)
				go SendData(conf, stream, sessions)
			}
		}()
	}

	// wait for the context to be cancelled
	<-ctx.Done()
	return nil
}

// processArgs processes functional args
func processArgs(opts []ClientOption) (*ClientConfig, error) {
	// default arguments
	opt := &ClientConfig{
		log: &log.Logger{
			Out: ioutil.Discard,
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

	return opt, nil
}

// WithServer configures the server this client uses
func WithServer(host string, p int) ClientOption {
	return func(opt *ClientConfig) error {
		opt.host = host
		opt.port = p
		return nil
	}
}

// WithTLS configures the tunnel to use TLS
// and sets the certificate expected, and a optional
// tls hostname override.
func WithTLS(cert, tlsHostOverride string) ClientOption {
	return func(opt *ClientConfig) error {
		if opt.certFile != "" {
			opt.TLS = true
		}
		opt.certFile = cert
		opt.tlsHostOverride = tlsHostOverride
		return nil
	}
}

// WithLogger sets the logger to be used by the server.
// if not set, output will be discarded
func WithLogger(l log.FieldLogger) ClientOption {
	return func(opt *ClientConfig) error {
		opt.log = l
		return nil
	}
}

// WithTunnels configures the tunnels to be exposed
// by this client. Each string should be in the format
// of: localPort:remotePort
func WithTunnels(scheme string, tunnels ...string) ClientOption {
	return func(opt *ClientConfig) error {
		opt.scheme = scheme
		opt.tunnels = tunnels
		return nil
	}
}

// ClientOption is an option able to be configured
type ClientOption func(*ClientConfig) error

// ClientConfig is a config object used to
// configure a GRPC tunnel from the client side.
// ClientOption should be used to modify this
type ClientConfig struct {
	host            string
	port            int
	TLS             bool
	certFile        string
	tlsHostOverride string
	scheme          string
	log             log.FieldLogger
	tunnels         []string
}
