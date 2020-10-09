package cmd

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/omrikiei/ktunnel/pkg/server"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var CertFile string
var KeyFile string

var serverCmd = &cobra.Command{
	Use:   "server [flags]",
	Short: "Run the ktunnel server(from remote - usually k8s pod)",
	Long:  `This command would start the tunnel server wait for tunnel clients to bind`,
	Example: `
# Run a ktunnel server(on a remote machine) on the non default port
ktunnel server -p 8181
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
		err := server.RunServer(ctx, &Port, &Tls, &KeyFile, &CertFile)
		if err != nil {
			log.Fatalf("Error running server: %v", err)
		}
	},
}

func init() {
	serverCmd.Flags().StringVar(&CertFile, "cert", "", "tls certificate file")
	serverCmd.Flags().StringVar(&KeyFile, "key", "", "tls key file")
	serverCmd.PersistentFlags().BoolVarP(&Verbose, "verbose", "v", false, "Emit debug logs")
	rootCmd.AddCommand(serverCmd)
}
