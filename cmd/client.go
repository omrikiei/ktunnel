package cmd

import (
	"ktunnel/pkg/client"
	"os"
	"os/signal"
	"sync"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var clientCmd = &cobra.Command{
	Use:   "client [flags] [ports]",
	Short: "Run the ktunnel client(from source listener - usually localhost)",
	Long:  `This command would open the tunnel to the server and forward tunnel ingress traffic to the the same port on localhost`,
	Args:  cobra.MinimumNArgs(1),
	Example: `
# Open a tunnel to a remote tunnel server
ktunnel client --host ktunnel-server.yourcompany.com -s tcp 8000 8001:8432
	`,
	Run: func(cmd *cobra.Command, args []string) {
		o := sync.Once{}
		closeChan := make(chan bool, 1)
		// Run tunnel client and establish connection

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)

		go func() {
			o.Do(func() {
				_ = <-sigs
				log.Info("Got exit signal, closing client tunnels")
				close(closeChan)
			})
		}()
		err := client.RunClient(&Host, &Port, Scheme, &Tls, &CaFile, &ServerHostOverride, args, closeChan, DialTimeout)
		if err != nil {
			log.Fatalf("Failed to run client: %v", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(clientCmd)
}
