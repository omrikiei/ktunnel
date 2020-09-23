package client

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/omrikiei/ktunnel/pkg/common"
	pb "github.com/omrikiei/ktunnel/tunnel_pb"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type Message struct {
	c *net.Conn
	d *[]byte
}

func ReceiveData(ctx context.Context, st pb.Tunnel_InitTunnelClient, sessionsOut chan<- *common.Session, port int32, scheme string) {
loop:
	for {
		select {
		case <-ctx.Done():
			log.WithError(ctx.Err()).Infof("stopping to receive data on port %d", port)
			_ = st.CloseSend()
			break loop
		default:
			log.Debugf("attempting to receive from stream")
			m, err := st.Recv()
			if err != nil {
				log.WithError(err).Warnf("error reading from stream")
				break loop
			}

			requestId, err := uuid.Parse(m.RequestId)
			if err != nil {
				log.WithError(err).WithField("session", m.RequestId).Errorf("failed parsing session uuid from stream, skipping")
			}

			session, exists := common.GetSession(requestId)
			if exists == false {
				log.WithFields(log.Fields{
					"session": m.RequestId,
					"port":    port,
				}).Infof("new connection")

				// new session
				conn, err := net.DialTimeout(strings.ToLower(scheme), fmt.Sprintf("localhost:%d", port), time.Millisecond*500)
				if err != nil {
					log.WithError(err).Errorf("failed connecting to localhost on port %d scheme %s", port, scheme)
					break loop
				}
				session = common.NewSessionFromStream(requestId, conn)
				go ReadFromSession(session, sessionsOut)
			} else if m.ShouldClose {
				session.Open = false
			}

			// process the data from the server
			handleStreamData(m, session)
		}
	}
}

func handleStreamData(m *pb.SocketDataResponse, session *common.Session) {
	if session.Open == false {
		log.WithField("session", session.Id).Infof("closed session")
		session.Close()
		return
	}

	data := m.GetData()
	log.WithField("session", session.Id).Infof("received %d bytes from server", len(data))
	if len(data) > 0 {
		session.Lock.Lock()
		log.WithField("session", session.Id).Infof("wrote %d bytes to conn", len(data))
		_, err := session.Conn.Write(data)
		session.Lock.Unlock()
		if err != nil {
			log.WithError(err).WithField("session", session.Id).Errorf("failed writing to socket, closing session")
			session.Close()
			return
		}
	}
}

func ReadFromSession(session *common.Session, sessionsOut chan<- *common.Session) {
	conn := session.Conn
	log.WithField("session", session.Id).Debugf("started reading conn")

	for {
		buff := make([]byte, common.BufferSize)
		br, err := conn.Read(buff)

		if err != nil {
			if err != io.EOF {
				log.WithError(err).WithField("session", session.Id).Errorf("failed reading from socket")
			}
			session.Open = false
			sessionsOut <- session
			break
		}

		log.WithField("session", session.Id).WithError(err).Infof("read %d bytes from conn", br)

		session.Lock.Lock()
		if br > 0 {
			log.WithField("session", session.Id).WithError(err).Infof("wrote %d bytes to session buf", br)
			_, err = session.Buf.Write(buff[0:br])
		}
		session.Lock.Unlock()

		if err != nil {
			log.WithField("session", session.Id).WithError(err).Errorf("failed writing to session buffer")
			break
		}
		sessionsOut <- session
	}
	log.Debugf("finished reading from session %s", session.Id)
}

func SendData(ctx context.Context, stream pb.Tunnel_InitTunnelClient, sessions <-chan *common.Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case session := <-sessions:

			// read the bytes from the buffer
			// but allow it to keep growing while we send the response
			session.Lock.Lock()
			bys := session.Buf.Len()
			bytes := make([]byte, bys)
			session.Buf.Read(bytes)

			log.WithField("session", session.Id).Infof("read %d from buffer out of %d available", len(bytes), bys)

			resp := &pb.SocketDataRequest{
				RequestId:   session.Id.String(),
				Data:        bytes,
				ShouldClose: !session.Open,
			}
			session.Lock.Unlock()

			log.WithFields(log.Fields{
				"session": session.Id,
				"close":   resp.ShouldClose,
			}).Infof("sending %d bytes to server", len(bytes))
			err := stream.Send(resp)
			if err != nil {
				log.WithError(err).Errorf("failed sending message to tunnel stream, exiting")
				return
			}
			log.WithFields(log.Fields{
				"session": session.Id,
				"close":   resp.ShouldClose,
			}).Infof("sent %d bytes to server", len(bytes))
		}
	}
}

func RunClient(ctx context.Context, host *string, port *int, scheme string, tls *bool, caFile, serverHostOverride *string, tunnels []string) error {
	var opts []grpc.DialOption
	if *tls {
		creds, err := credentials.NewClientTLSFromFile(*caFile, *serverHostOverride)
		if err != nil {
			log.Fatalf("Failed to create TLS credentials %v", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", *host, *port), opts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewTunnelClient(conn)
	for _, rawTunnelData := range tunnels {
		tunnelData, err := common.ParsePorts(rawTunnelData)
		if err != nil {
			log.Error(err)
		}
		go func() {
			log.Println(fmt.Sprintf("starting %s tunnel from source %d to target %d", scheme, tunnelData.Source, tunnelData.Target))
			tunnelScheme, ok := pb.TunnelScheme_value[scheme]
			if ok != false {
				log.Fatalf("unsupported connection scheme %s", scheme)
			}

			req := &pb.SocketDataRequest{
				Port:     tunnelData.Source,
				LogLevel: 0,
				Scheme:   pb.TunnelScheme(tunnelScheme),
			}

			stream, err := client.InitTunnel(ctx)
			if err != nil {
				log.Errorf("Error sending init tunnel request: %v", err)
			} else {
				err := stream.Send(req)
				if err != nil {
					log.WithError(err).Errorf("Failed to send initial tunnel request to server")
				} else {
					sessions := make(chan *common.Session)
					go ReceiveData(ctx, stream, sessions, tunnelData.Target, scheme)
					go SendData(ctx, stream, sessions)
					<-ctx.Done()
				}
			}

		}()
	}

	<-ctx.Done()
	return nil
}
