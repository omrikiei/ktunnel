package main

import (
	"flag"
	log "github.com/sirupsen/logrus"
	"ktunnel/client"
	"ktunnel/injector"
	"ktunnel/server"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
)

var (
	serverCmd  = flag.NewFlagSet("server", flag.ExitOnError)
	clientCmd  = flag.NewFlagSet("client", flag.ExitOnError)
	o = sync.Once{}
)

func main() {
	flag.Parse()
	if len(os.Args) < 2 {
		log.Error("Expected 'client' or 'server' subcommands")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "client":
		host := clientCmd.String("host", "localhost", "Host to connect to")
		caFile := clientCmd.String("ca_file", "", "TLS cert auth file")
		scheme := clientCmd.String("scheme", "tcp", "Connection scheme")
		deploymentName := clientCmd.String("deployment", "", "Name of the kubernetes deployment")
		namespace := clientCmd.String("namespace", "default", "kubernetes namespace")
		serverHostOverride := clientCmd.String("server_host_override", "", "Server name use to verify the hostname returned by TLS handshake")
		clientPort       := clientCmd.Int("port", 28688, "Server port")
		clientTls        := clientCmd.Bool("tls", false, "Connection uses TLS if true, else plain TCP")
		err := clientCmd.Parse(os.Args[2:])
		if err != nil {
			log.Fatal(err)
		}
		if *deploymentName == "" {
			log.Fatalf("deployment cannot be empty")
			os.Exit(1)
		}
		// Inject sidecar server to deployment
		readyChan := make(chan bool, 1)
		_, err = injector.InjectSidecar(namespace, deploymentName, clientPort, readyChan)
		if err != nil {
			log.Fatalf("failed injecting sidecar: %v", err)
		}
		log.Info("Waiting for deployment to be ready")
		<- readyChan
		sigs := make(chan os.Signal, 1)
		done := make(chan bool, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)

		go func() {
			o.Do(func(){
				_ = <-sigs
				ok, err := injector.RemoveSidecar(namespace, deploymentName)
				if !ok {
					log.Errorf("Failed removing tunnel sidecar", err)
				}
				done<-true
			})
		}()

		// Create a port forward to the deployment/pod
		strPort := strconv.FormatInt(int64(*clientPort), 10)
		fwdChan := make(chan bool, 1)
		_, err = injector.PortForward(namespace, deploymentName, &[]string{strPort}, fwdChan)
		if err != nil {
			log.Fatalf("Failed to run port forwarding: %v", err)
			os.Exit(1)
		}
		log.Info("Waiting for port forward to finish")
		<-fwdChan
		// Run tunnel client and establish connection
		err = client.RunClient(host, clientPort, *scheme ,clientTls, caFile, serverHostOverride, flag.Args()[2:])
		if err != nil {
			log.Fatalf("Failed to run client: %v", err)
			os.Exit(1)
		}
		<-done
		os.Exit(0)
	case "server":
		certFile   := serverCmd.String("cert_file", "", "TLS cert file")
		keyFile    := serverCmd.String("key_file", "", "TLS key file")
		serverPort := serverCmd.Int("port", 28688, "Server port")
		serverTls := serverCmd.Bool("tls", false, "Connection uses TLS if true, else plain TCP")
		err := serverCmd.Parse(os.Args[2:])
		if err != nil {
			log.Fatal(err)
		}
		err = server.RunServer(serverPort, serverTls, keyFile, certFile)
		if err != nil {
			log.Fatalf("Error running server: %v", err)
		}
		os.Exit(0)
	default:
		log.Fatalf("Expected 'client' or 'server' subcommands")
		os.Exit(1)
	}
}
