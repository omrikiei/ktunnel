package server

import (
	"fmt"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	pb "ktunnel/tunnel_pb"
	"net/http"
)

func startWebsocketTunnel(stream pb.Tunnel_InitTunnelServer, port int32) error {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  bufferSize,
		WriteBufferSize: bufferSize,
	}
	http.HandleFunc("/", wsHandlerClosure(upgrader, stream))
	addr := fmt.Sprintf(":%d", port)
	log.Infof("listening on %s", addr)
	return http.ListenAndServe(addr, nil)
}

func wsHandlerClosure(upgrader websocket.Upgrader, stream pb.Tunnel_InitTunnelServer) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("upgrade:", err)
			return
		}
		closeChan := make(chan bool, 1)
		defer func() {
			<-closeChan
			err := c.Close()
			if err != nil {
				log.Errorf("Failed closing websocket connection: %v", err)
			}
		}()
		go func() {
			for {
				var res pb.SocketDataResponse
				mt, message, err := c.ReadMessage()
				if err != nil {
					log.Println("read:", err)
					res.ShouldClose = true
					_ = stream.Send(&res)
					closeChan<-true
					break
				}
				res.Data = message
				res.MessageType = int32(mt)
				res.Path = r.URL.Path
				err = stream.Send(&res)
				if err != nil {
					log.Errorf("Failed sending ws request to stream: %v", err)
					closeChan<-true
					break
				}
			}
		}()
		go func() {
			for {
				req, err := stream.Recv()
				if err != nil {
					log.Errorf("Failed reading message from stream: %v", err)
					closeChan<-true
					break
				}
				mt := int(req.MessageType)
				message := req.Data
				err = c.WriteMessage(mt, message)
				if err != nil {
					log.Errorf("Failed writing message from stream to websocket: %v", err)
					closeChan<-true
					break
				}
			}
		}()
	}
}