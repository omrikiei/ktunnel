package client

import (
	"errors"
	"fmt"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"kube_tunnel/common"
	pb "kube_tunnel/tunnel_pb"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
)

type redirectRequest struct {
	source int32
	target int32
}

type Message struct {
	c *net.Conn
	d *[]byte
}

func datagramResponse(size int, data *[]byte) *pb.SocketDataRequest {
	log.Printf("preparing to send %d bytes to server", size)
	return &pb.SocketDataRequest{
		Data:                 (*data)[:size],
	}
}

func parsePorts(s string) (error, *redirectRequest) {
	raw := strings.Split(s, ":")
	if len(raw) == 0 {
		return errors.New(fmt.Sprintf("Failed parsing redirect request: %s", s)), nil
	}
	if len(raw) == 1 {
		p, err := strconv.ParseInt(raw[0], 10, 32)
		if err != nil {
			return errors.New(fmt.Sprintf("failed to parse port %s, %v", raw[0], err)), nil
		}
		return nil, &redirectRequest{
			int32(p),
			int32(p),
		}
	}
	if len(raw) == 2 {
		s, err := strconv.ParseInt(raw[0], 10, 32)
		if err != nil {
			return errors.New(fmt.Sprintf("failed to parse port %s, %v", raw[0], err)), nil
		}
		t, err := strconv.ParseInt(raw[1], 10, 32)
		if err != nil {
			return errors.New(fmt.Sprintf("failed to parse port %s, %v", raw[1], err)), nil
		}
		return nil, &redirectRequest{
			source: int32(s),
			target: int32(t),
		}
	}
	return errors.New(fmt.Sprintf("Error, bad tunnel format: %s", s)), nil
}

func RunClient(host *string, port *int, tls *bool, caFile, serverHostOverride *string, tunnels []string) error {
	wg := sync.WaitGroup{}
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
		err, tunnelData := parsePorts(rawTunnelData)
		if err != nil {
			log.Println(err)
		}
		wg.Add(1)
		go func() {
			log.Println(fmt.Sprintf("Starting tunnel from source %d to target %d", tunnelData.source, tunnelData.target))
			ctx := context.Background()
			req := &pb.SocketDataRequest{
				Port:                 tunnelData.source,
				LogLevel:             0,
				Scheme:                 pb.TunnelScheme_TCP,
			}
			stream, err := client.InitTunnel(ctx)
			if err != nil {
				log.Println(fmt.Sprintf("Error sending init tunnel request: %v", err))
			} else {
				err := stream.Send(req)
				if err != nil {
					log.Println("Failed to send initial tunnel request to server")
				} else {
					serverReceiver := make(chan *Message)
					closeStream := make(chan bool, 1)
					go func() {
						for {
							serverStream, err := stream.Recv()
							if err != nil {
								log.Println("Error: failed to get response from server: ", err)
								serverReceiver <- nil
								return
							}
							d := serverStream.GetData()
							log.Printf("got %d bytes from server", len(d))
							serverReceiver <- &Message{
								nil,
								&d,
							}
						}
					}()

					go func() {
						for {
							serverResponse := <- serverReceiver
							if serverResponse == nil {
								log.Println("Client exit")
								closeStream <- true
								return
							} else {
								ln, err := net.Dial(strings.ToLower(req.Scheme.String()), fmt.Sprintf("localhost:%d", tunnelData.target))
								if err != nil {
									log.Println(fmt.Sprintf("Error connecting to localhost:%d", tunnelData.target))
									return
								}
								_, err = ln.Write(*serverResponse.d)
								if err != nil {
									fmt.Println("Error; failed writing to socket: ", err)
									_ = ln.Close()
								}
								br, buff, err := common.StreamToByte(ln)
								if err != nil {
									log.Println("Error: failed to read line from local socket: ", err)
									return
								}
								if br > 0 {
									err := stream.Send(datagramResponse(len(*buff), buff))
									if err != nil {
										fmt.Println("Error; failed writing to client: ", err)
									}
								}
								_ = ln.Close()

							}
						}
					} ()
					<- closeStream
					_ = stream.CloseSend()
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()
	log.Println("All ports closed, exiting..")
	return nil
}