package cmd

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"ktunnel/pkg/client"
	"ktunnel/pkg/k8s"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
)

var exposeCmd = &cobra.Command{
	Use:   "expose [flags] SERVICE_NAME [ports]",
	Short: "Expose local machine as a service on the kubernetes cluster",
	Long:  `This command would inject a new service and deployment to the cluster, and open the tunnel to the server 
			forwarding tunnel ingress traffic to the the same port on localhost`,
	Args:  cobra.MinimumNArgs(2),
	Example: `
# Expose a local application running on port 8000 via http
ktunnel expose kewlapp 80:8000
			  
# Expose a local redis server
ktunnel expose redis 6379
              `,
	Run: func(cmd *cobra.Command, args []string) {
		// Create service and deployment
		svcName, ports := args[0], args[1:]
		readyChan := make(chan bool, 1)
		err := k8s.ExposeAsService(&Namespace, &svcName, Port, Scheme, ports, readyChan)
		if err != nil {
			log.Fatalf("Failed to expose local machine as a service: %v", err)
		}
		sigs := make(chan os.Signal, 1)
		wg := &sync.WaitGroup{}
		done := make(chan bool, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)

		// Teardown
		go func() {
			o.Do(func() {
				_ = <-sigs
				log.Info("Got exit signal, closing client tunnels and removing k8s objects")
				CloseChan <- true
				err := k8s.TeardownExposedService(&Namespace, &svcName)
				if err != nil {
					log.Errorf("Failed deleting k8s objects: %s", err)
				}
				done <- true
			})
		}()

		log.Info("waiting for deployment to be ready")
		<- readyChan

		// Port-Forward
		strPort := strconv.FormatInt(int64(Port), 10)
		stopChan := make(chan struct{}, 1)
		// Create a tunnel client for each replica
		sourcePorts, err := k8s.PortForward(&Namespace, &svcName, strPort, wg, stopChan)
		if err != nil {
			log.Fatalf("Failed to run port forwarding: %v", err)
			os.Exit(1)
		}
		log.Info("Waiting for port forward to finish")
		wg.Wait()
		for _, srcPort := range *sourcePorts {
			go func() {
				p, err := strconv.ParseInt(srcPort, 10, 0)
				if err != nil {
					log.Fatalf("Failed to run client: %v", err)
				}
				prt := int(p)
				err = client.RunClient(&Host, &prt, Scheme ,&Tls, &CaFile, &ServerHostOverride, args[1:], CloseChan)
				if err != nil {
					log.Fatalf("Failed to run client: %v", err)
				}
			}()
		}
		_ = <-done
	},
}

func init() {
	exposeCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	exposeCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	exposeCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	exposeCmd.Flags().StringVarP(&Namespace, "namespace","n",  "default", "Namespace")
	rootCmd.AddCommand(exposeCmd)
}
