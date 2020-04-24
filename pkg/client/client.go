package client

import (
	"fmt"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io"
	"ktunnel/pkg/common"
	pb "ktunnel/tunnel_pb"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	bufferSize = 1024*32
)

type Message struct {
	c *net.Conn
	d *[]byte
}

func ReceiveData(st *pb.Tunnel_InitTunnelClient, closeStream <-chan bool, sessionsOut chan<- *common.Session, port int32, scheme string) {
	stream := *st
loop:
	for {
		select {
		case <-closeStream:
			log.Infof("stopping to receive data on port %d", port)
			_ = stream.CloseSend()
			break loop
		default:
			log.Debugf("attempting to receive from stream")
			m, err := stream.Recv()
			if err != nil {
				log.Warnf("error reading from stream; %v", err)
				break loop
			}
			log.Debugf("%s; got session from server: %s", m.RequestId, m.GetData())
			requestId, err := uuid.Parse(m.RequestId)
			if err != nil {
				log.Errorf("%s; failed parsing session uuid from stream, skipping", m.RequestId)
			}
			session, exists := common.GetSession(requestId)
			if exists == false {
				if m.ShouldClose != true {
					log.Infof("%s; new session; connecting to port %d", m.RequestId, port)
					// new session
					conn, err := net.DialTimeout(strings.ToLower(scheme), fmt.Sprintf("localhost:%d", port), time.Millisecond*50)
					if err != nil {
						log.Errorf("failed connecting to localhost on port %d scheme %s", port, scheme)
						return
					}
					session = common.NewSessionFromStream(requestId, conn)
					go ReadFromSession(session, sessionsOut)
				} else {
					session = common.NewSessionFromStream(requestId, nil)
					session.Open = false
				}
			}
			handleStreamData(m, session)
		}
	}
}

func handleStreamData(m *pb.SocketDataResponse, session *common.Session) {
	if session.Open == false {
		if session.Conn != nil {
			ok, err := common.CloseSession(session.Id)
			if ok != true {
				log.Printf("%s; failed closing session: %v", session.Id.String(), err)
			}
		}
	} else {
		c := session.Conn
		data := m.GetData()
		log.Debugf("%s got %d bytes", session.Id, len(data))
		if len(data) > 0 {
			session.Lock.Lock()
			_, err := c.Write(data)
			session.Lock.Unlock()
			if err != nil {
				log.Printf("%s; failed writing to socket, closing session: %v", session.Id.String(), err)
				ok, err := common.CloseSession(session.Id)
				if ok != true {
					log.Printf("%s; failed closing session: %v", session.Id.String(), err)
				}
			}
		}
	}
}

func ReadFromSession(session *common.Session, sessionsOut chan<- *common.Session) {
	conn := session.Conn
	log.Debugf("started reading from session %s", session.Id)
	buff := make([]byte, bufferSize)
	for {
		//_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		br, err := conn.Read(buff)
		if err != nil {
			if err != io.EOF {
				log.Errorf("%s; failed reading from socket, exiting: %v", session.Id.String(), err)
			}
			session.Open = false
			sessionsOut <- session
			break
		}
		log.Debugf("read %d bytes from conn on session %s; err: %v", br, session.Id, err)
		log.Debugf("locking: %s", session.Id)
		if br > 0 {
			session.Lock.Lock()
			_, err = session.Buf.Write(buff[:br])
			session.Lock.Unlock()
			if br == len(buff) {
				newSize := len(buff)*2
				log.Infof("increasing buffer size to %d", newSize)
				buff = make([]byte, newSize)
			}
		}
		log.Debugf("unlocked: %s", session.Id)
		if err != nil {
			log.Errorf("%s; failed writing to session buffer: %v", session.Id, err)
			break
		}
		sessionsOut <- session
	}
	log.Debugf("finished reading from session %s", session.Id)
}

func SendData(stream *pb.Tunnel_InitTunnelClient, sessions <-chan *common.Session, closeChan <-chan bool) {
	for {
		select {
		case <-closeChan:
			return
		case session := <-sessions:
			session.Lock.Lock()
			st := *stream
			resp := &pb.SocketDataRequest{
				RequestId:   session.Id.String(),
				Data:        session.Buf.Bytes(),
				ShouldClose: false,
			}
			if session.Open == false {
				resp.ShouldClose = true
			}
			session.Buf.Reset()
			session.Lock.Unlock()
			go func() {
				err := st.Send(resp)
				if err != nil {
					log.Errorf("failed sending message to tunnel stream, exiting; %v", err)
				}
			}()
		}
	}
}

func RunClient(host *string, port *int, scheme string, tls *bool, caFile, serverHostOverride *string, tunnels []string, stopChan <-chan bool) error {
	wg := sync.WaitGroup{}
	closeStreams := make([]chan bool, len(tunnels))
	go func() {
		<-stopChan
		for _, c := range closeStreams {
			close(c)
		}
	}()
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
	defer func() {
		_ = conn.Close()
	}()
	client := pb.NewTunnelClient(conn)
	for i, rawTunnelData := range tunnels {
		tunnelData, err := common.ParsePorts(rawTunnelData)
		if err != nil {
			log.Error(err)
		}
		wg.Add(1)
		c := make(chan bool, 1)
		go func(closeStream chan bool) {
			log.Println(fmt.Sprintf("starting %s tunnel from source %d to target %d", scheme, tunnelData.Source, tunnelData.Target))
			ctx := context.Background()
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
					log.Errorf("Failed to send initial tunnel request to server")
				} else {
					sessions := make(chan *common.Session)
					go ReceiveData(&stream, closeStream, sessions, tunnelData.Target, scheme)
					go SendData(&stream, sessions, closeStream)
					<-closeStream
				}
			}
			wg.Done()
		}(c)
		closeStreams[i] = c
	}
	wg.Wait()
	return nil
}
