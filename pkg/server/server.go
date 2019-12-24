package server

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	pb "ktunnel/tunnel_pb"
	"net"
)

const (
	bufferSize = 1024
)

type tunnelServer struct {}

func NewServer() *tunnelServer {
	return &tunnelServer{}
}

func (t *tunnelServer) InitTunnel(stream pb.Tunnel_InitTunnelServer) error {
	request, err := stream.Recv()
	if err != nil {
		log.Fatalf("Failed receiving initial connection from tunnel")
		return err
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
	scheme := request.GetScheme()
	log.Infof("Opening %s connection on port %d", scheme, port)
	if scheme == pb.TunnelScheme_WEBSOCKET {
		log.Info("Starting websocket server")
		return startWebsocketTunnel(stream, port)
	} else {
		log.Infof("Starting %s server", scheme.String())
		return startHttpTunnel(stream, &scheme, port)
	}
}

func RunServer(port *int, tls *bool, keyFile, certFile *string) error {
	log.Infof("Starting to listen on port %d", *port)
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