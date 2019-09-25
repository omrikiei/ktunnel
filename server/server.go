package server

import (
	"errors"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"kube_tunnel/common"
	pb "kube_tunnel/tunnel_pb"
	"log"
	"net"
	"strings"
)

const (
	bufferSize = 32*1024
)

type tunnelServer struct {}

type Message struct {
	c *net.Conn
	d *[]byte
}

func NewServer() *tunnelServer {
	return &tunnelServer{}
}

func datagramResponse(size int, data *[]byte) *pb.SocketDataResponse {
	log.Printf("preparing to send %d bytes to client", size)
	return &pb.SocketDataResponse{
		HasErr:               false,
		LogMessage:           &pb.LogMessage{
			LogLevel:             pb.LogLevel_DEBUG,
			Message:              fmt.Sprintf("read %d bytes", size),
		},
		Data:                 (*data)[:size],
	}
}

func (t *tunnelServer) InitTunnel(stream pb.Tunnel_InitTunnelServer) error {
	request, err := stream.Recv()
	if err != nil {
		log.Fatalf("Failed to receive initial connection from tunnel")
	}
	port := request.GetPort()
	if port == 0 {
		err := stream.Send(&pb.SocketDataResponse{
			HasErr: true,
			LogMessage: &pb.LogMessage{
				LogLevel:             pb.LogLevel_ERROR,
				Message:              "missing port",
			},
		})
		if err != nil {
			return err
		}
		return errors.New("missing port")
	}
	log.Printf("Opening %s conenction on port %d", request.GetScheme(), port)
	ln, err := net.Listen(strings.ToLower(request.GetScheme().String()), fmt.Sprintf(":%d", port))
	if err != nil {
		defer func() {
			log.Printf("Failed listening on port %d: %v", port, err)
		}()
		_ = stream.Send(&pb.SocketDataResponse{
			HasErr: true,
			LogMessage: &pb.LogMessage{
				LogLevel:             pb.LogLevel_ERROR,
				Message:              fmt.Sprintf("failed opening listener type %s on port %d: %v", request.GetScheme(), request.GetPort(), err),
			},
		})
		return err
	}
	defer func(){
		log.Printf("Closing conenction on port %d", port)
		_ = ln.Close()
	}()

	socketReceiver := make(chan *Message)
	clientReceiver := make(chan *Message)
	connClose := make(chan bool, 1)

	go func() {
		for {
			select {
			case socketMessage := <-socketReceiver:
				if socketMessage == nil {
					log.Println("Socket exit")
					connClose <- true
					break
				}
				data := *socketMessage.d
				err := stream.Send(datagramResponse(len(data), &data))
				if err != nil {
					log.Println("Error; failed writing to client: ", err)
					connClose <- true
					break
				}

			case clientResponse := <-clientReceiver:
				if clientResponse == nil {
					log.Println("Client exit")
					break
				}
				//fmt.Println("Writing to socket: ", clientResponse)
				c := *clientResponse.c
				_, err := c.Write(*clientResponse.d)
				if err != nil {
					log.Println("Error; failed writing to socket: ", err)
				}
				connClose <- true
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Failed accepting connection on port %d", port)
			return err
		}
		go func(c *net.Conn) {
			_, buff, err := common.StreamToByte(*c)
			if err != nil {
				log.Println("Error: failed to read line from connection: ", err)
				socketReceiver <- nil
				return
			}
			if len(*buff) > 0 {
				socketReceiver <- &Message{
					c,
					buff,
				}
			}
		}(&conn)

		go func(c *net.Conn) {
			for {
				clientStream, err := stream.Recv()
				if err != nil {
					log.Println("Error: failed to get response from client: ", err)
					clientReceiver <- nil
					return
				}
				if len(clientStream.Data) > 0 {
					data := clientStream.GetData()
					clientReceiver <- &Message{
						c,
						&data,
					}
					break
				}
				fmt.Println("end of stream receive: ", clientStream)
			}
		}(&conn)
		<-connClose
		_ = conn.Close()
	}

	return nil
}

func RunServer(port *int, tls *bool, keyFile, certFile *string) error {
	log.Println(fmt.Sprintf("Starting to listen on port %d", *port))
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