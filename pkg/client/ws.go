package client

import (
	"fmt"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	pb "ktunnel/tunnel_pb"
	"net/url"
)

func localWsConnection(st *pb.Tunnel_InitTunnelClient, closeStream chan<-bool, port int32) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	closeChan := make(chan bool, 1)
	go func() {
		// Waiting for initial connect
		stream := *st
		r, err := stream.Recv()
		if err != nil {
			log.Errorf("Failed getting request from stream: %v", err)
			return
		}
		u := url.URL{Scheme: "ws", Host: addr, Path: r.RequestId}
		log.Infof("connecting to %s", u.String())

		c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Errorf("failed connecting to local websocket %s: %v", u.String(), err)
			return
		}
		defer func() {
			<-closeChan
			err := c.Close()
			if err != nil {
				log.Errorf("Failed closing websocket connection: %v", err)
			}
			closeStream<-true
		}()
		// Receiver
		go func() {
			for {
				m, err := stream.Recv()
				if err != nil {
					log.Errorf("Failed reading from stream: %v", err)
					closeChan<-true
					break
				}
				err = c.WriteMessage(int(m.MessageType), m.Data)
				if err != nil {
					log.Errorf("Couldn't write message to local websocket: %v", err)
					closeChan<-true
					break
				}
			}
		}()

		// Sender
		go func() {
			for {
				mt, message, err := c.ReadMessage()
				req := pb.SocketDataRequest{}
				if err != nil {
					log.Errorf("Failed reading from socket: %v", err)
					closeChan<-true
					req.ShouldClose = true
					_ = stream.Send(&req)
					break
				}
				req.MessageType = int32(mt)
				req.Data = message
				err = stream.Send(&req)
				if err != nil {
					log.Errorf("Failed writing to stream: %v", err)
					closeChan<-true
					break
				}
			}
		}()
	}()
}