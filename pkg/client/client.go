package client

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"ktunnel/pkg/common"
	pb "ktunnel/tunnel_pb"
	"strings"
	"sync"
)

const (
	bufferSize = 1024
)

func RunClient(host *string, port *int, scheme string, tls *bool, caFile, serverHostOverride *string, tunnels []string, stopChan <-chan bool) error {
	wg := sync.WaitGroup{}
	closeStreams := make([]chan bool, len(tunnels))
	go func() {
		<-stopChan
		for _, c := range closeStreams {
			c <- true
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
	for _, rawTunnelData := range tunnels {
		tunnelData, err := common.ParsePorts(rawTunnelData)
		if err != nil {
			log.Error(err)
		}
		wg.Add(1)
		c := make(chan bool, 1)
		go func(closeStream chan bool) {
			log.Println(fmt.Sprintf("starting %s tunnel from source %d to target %d", scheme, tunnelData.Source, tunnelData.Target))
			ctx := context.Background()
			tunnelScheme, ok := pb.TunnelScheme_value[strings.ToUpper(scheme)]
			if ok == false {
				log.Fatalf("unsupported connection scheme %s", scheme)
			}
			req := &pb.SocketDataRequest{
				Port:                 tunnelData.Source,
				LogLevel:             0,
				Scheme:               pb.TunnelScheme(tunnelScheme),
			}
			stream, err := client.InitTunnel(ctx)
			if err != nil {
				log.Errorf("Error sending init tunnel request: %v", err)
			} else {
				err := stream.Send(req)
				if err != nil {
					log.Errorf("Failed to send initial tunnel request to server")
				} else {
					requests := make(chan *common.Request)
					if strings.ToUpper(scheme) == "WEBSOCKET" {
						go localWsConnection(&stream, closeStream, tunnelData.Target)
					} else {
						go receiveData(&stream, closeStream, requests, tunnelData.Target, scheme)
						go sendData(requests, &stream)
					}
					<- closeStream
					_ = stream.CloseSend()
				}
			}
			wg.Done()
		}(c)
		closeStreams = append(closeStreams, c)
	}
	wg.Wait()
	return nil
}