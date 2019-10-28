package cmd

import (
	"github.com/spf13/cobra"
	"ktunnel/pkg/server"
	"log"
)

var CertFile string
var KeyFile string

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run the ktunnel server(from remote - usually k8s pod)",
	Long:  `This command would start the tunnel server wait for tunnel clients to bind`,
	Run: func(cmd *cobra.Command, args []string) {
		err := server.RunServer(&Port, &Tls, &KeyFile, &CertFile)
		if err != nil {
			log.Fatalf("Error running server: %v", err)
		}
	},
}

func init() {
	serverCmd.Flags().StringVar(&CertFile, "cert", "", "tls certificate file")
	serverCmd.Flags().StringVar(&KeyFile, "key", "", "tls key file")
	rootCmd.AddCommand(serverCmd)
}