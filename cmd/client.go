package cmd

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/omrikiei/ktunnel/pkg/client"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var Host string
var CaFile string
var Scheme string
var ServerHostOverride string

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
		ctx, cancel := context.WithCancel(context.Background())
		if Verbose {
			log.SetLevel(log.DebugLevel)
		}
		o := sync.Once{}
		// Run tunnel client and establish connection

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)
		go func() {
			o.Do(func() {
				_ = <-sigs
				log.Info("Got exit signal, closing client tunnels")
				cancel()
			})
		}()

		err := client.RunClient(ctx, &Host, &Port, Scheme, &Tls, &CaFile, &ServerHostOverride, args)
		if err != nil {
			log.Fatalf("Failed to run client: %v", err)
		}
	},
}

func init() {
	clientCmd.Flags().StringVarP(&Host, "host", "H", "localhost", "server host address")
	clientCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	clientCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	clientCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	clientCmd.PersistentFlags().BoolVarP(&Verbose, "verbose", "v", false, "Emit debug logs")
	rootCmd.AddCommand(clientCmd)
}
