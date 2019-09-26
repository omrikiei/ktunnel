package main

import (
	"flag"
	log "github.com/sirupsen/logrus"
	"ktunnel/client"
	"ktunnel/server"
	"os"
)

var (
	serverCmd  = flag.NewFlagSet("server", flag.ExitOnError)
	certFile   = serverCmd.String("cert_file", "", "TLS cert file")
	keyFile    = serverCmd.String("key_file", "", "TLS key file")
	clientCmd  = flag.NewFlagSet("client", flag.ExitOnError)
	host = clientCmd.String("host", "localhost", "Host to connect to")
	caFile = clientCmd.String("ca_file", "", "TLS cert auth file")
	scheme = clientCmd.String("scheme", "tcp", "Connection scheme")
	serverHostOverride = clientCmd.String("server_host_override", "", "Server name use to verify the hostname returned by TLS handshake")
	port       = flag.Int("port", 28688, "Server port")
	tls        = flag.Bool("tls", false, "Connection uses TLS if true, else plain TCP")
)

func main() {
	flag.Parse()
	if len(os.Args) < 2 {
		log.Error("Expected 'client' or 'server' subcommands")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "client":
		err := client.RunClient(host, port, *scheme ,tls, caFile, serverHostOverride, flag.Args()[1:])
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
		log.Fatalf("Expected 'client' or 'server' subcommands")
		os.Exit(1)
	}
}
