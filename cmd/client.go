package cmd

import (
	"github.com/spf13/cobra"
	"ktunnel/pkg/client"
	"log"
)

var Host string
var CaFile string
var Scheme string
var ServerHostOverride string

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "Run the ktunnel client(from source listener - usually localhost)",
	Long:  `This command would open the tunnel to the server and forward tunnel ingress traffic to the the same port on localhost`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// Run tunnel client and establish connection
		err := client.RunClient(&Host, &Port, Scheme ,&Tls, &CaFile, &ServerHostOverride, args)
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
	rootCmd.AddCommand(clientCmd)
}