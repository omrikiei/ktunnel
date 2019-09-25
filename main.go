package main

import (
	"flag"
	"fmt"
	"kube_tunnel/server"
	"kube_tunnel/client"
	"log"
	"os"
)

var (
	serverCmd  = flag.NewFlagSet("server", flag.ExitOnError)
	certFile   = serverCmd.String("cert_file", "", "The TLS cert file")
	keyFile    = serverCmd.String("key_file", "", "The TLS key file")
	clientCmd  = flag.NewFlagSet("client", flag.ExitOnError)
	host = clientCmd.String("host", "localhost", "The host to connect to")
	caFile = clientCmd.String("ca_file", "", "The TLS cert auth file")
	serverHostOverride = clientCmd.String("server_host_override", "", "The server name use to verify the hostname returned by TLS handshake")
	port       = flag.Int("port", 28688, "The server port")
	tls        = flag.Bool("tls", false, "Connection uses TLS if true, else plain TCP")
)

func main() {
	flag.Parse()
	if len(os.Args) < 2 {
		fmt.Println("Expected 'client' or 'server' subcommands")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "client":
		err := client.RunClient(host, port, tls, caFile, serverHostOverride, flag.Args()[1:])
		if err != nil {
			log.Fatalf("Failed to run client: %v", err)
		}
		os.Exit(0)
	case "server":
		err := server.RunServer(port, tls, keyFile, certFile)
		if err != nil {
			log.Fatalf("Error running server: %v", err)
		}
		os.Exit(0)
	default:
		fmt.Println("Expected 'client' or 'server' subcommands")
		os.Exit(1)
	}
}
