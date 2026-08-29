// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"context"

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
		if verbose {
			logger.SetLevel(log.DebugLevel)
		}
		// Nothing was created in the cluster, so there is nothing to tear
		// down; the session is here for the signal handling and the context
		// the supervisor runs under.
		sess := newTunnelSession(ctx, cancel, "Got exit signal, closing client tunnels", nil)
		defer sess.finish()

		supervise(sess, tunnelClientAttempt(Host, port, args))
	},
}

func init() {
	clientCmd.Flags().StringVarP(&Host, "host", "H", "localhost", "server host address")
	clientCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	clientCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	clientCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	addReconnectFlags(clientCmd)
	rootCmd.AddCommand(clientCmd)
}
