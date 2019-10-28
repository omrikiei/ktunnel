package cmd

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"ktunnel/pkg/injector"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
)

var Namespace string
var o sync.Once

var injectCmd = &cobra.Command{
	Use:   "inject",
	Short: "Inject server sidecar to the cluster and run the ktunnel client to establish a connection",
	Long:  `This command accepts a pod/deployment and injects the tunnel sidecar to that artifact, 
			it then establishes a reverse tunnel`,
}

var injectPodCmd = &cobra.Command{
	Use: "pod",
	Short: "Inject server sidecar to a pod and run the ktunnel client to establish a connection",
	Args: cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {

	},
}
var injectDeploymentCmd = &cobra.Command{
	Use: "deployment",
	Short: "Inject server sidecar to a deployment and run the ktunnel client to establish a connection",
	Args: cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		// Inject
		deployment := args[0]
		readyChan := make(chan bool, 1)
		_, err := injector.InjectSidecar(&Namespace, &deployment, &Port, readyChan)
		if err != nil {
			log.Fatalf("failed injecting sidecar: %v", err)
		}
		log.Info("Waiting for deployment to be ready")
		<- readyChan

		// Signal hook to remove sidecar
		sigs := make(chan os.Signal, 1)
		done := make(chan bool, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)

		go func() {
			o.Do(func(){
				_ = <-sigs
				ok, err := injector.RemoveSidecar(&Namespace, &deployment)
				if !ok {
					log.Errorf("Failed removing tunnel sidecar", err)
				}
				done<-true
			})
		}()

		// Port-Forward
		strPort := strconv.FormatInt(int64(Port), 10)
		fwdChan := make(chan bool, 1)
		_, err = injector.PortForward(&Namespace, &deployment, &[]string{strPort}, fwdChan)
		if err != nil {
			log.Fatalf("Failed to run port forwarding: %v", err)
			os.Exit(1)
		}
		log.Info("Waiting for port forward to finish")
		<-fwdChan
		// Run tunnel client
		clientCmd.Run(cmd, args[1:])
		<-done
	},
}

func init() {
	injectCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	injectCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	injectCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	injectCmd.Flags().StringVarP(&Namespace, "namespace","n",  "default", "Namespace")
	injectCmd.AddCommand(injectPodCmd, injectDeploymentCmd)
	rootCmd.AddCommand(injectCmd)
}
