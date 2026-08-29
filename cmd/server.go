package cmd

import (
	"context"
	"os"

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
		if verbose {
			logger.SetLevel(log.DebugLevel)
		}

		// The same signal handling as every other command, which this one
		// was left out of: the first signal asks the server to stop, and a
		// second kills the process. The handler it replaces wrapped a
		// blocking receive in a sync.Once -- so the Once guarded nothing,
		// and every signal after the first was swallowed. That escape hatch
		// matters more here than anywhere else now, because stopping waits
		// for every open tunnel's handler to return, which is a strictly
		// longer shutdown than closing a listener was.
		//
		// No teardown: the server creates nothing outside its own process.
		sess := newTunnelSession(ctx, cancel, "Got exit signal, stopping the tunnel server", nil)
		defer sess.finish()

		config := []server.Option{server.WithPort(port), server.WithLogger(&logger)}
		if tls {
			config = append(config, server.WithTLS(CertFile, KeyFile))
		}
		if err := server.RunServer(sess.ctx, config...); err != nil {
			// Not log.Fatal: that skips every deferred function, and this
			// one exits through the session like the other commands do.
			// RunServer reports a shutdown the user asked for as nil, so
			// reaching here is a genuine failure.
			logger.WithError(err).Error("error running server")
			sess.finish()
			os.Exit(1)
		}
	},
}

func init() {
	serverCmd.Flags().StringVar(&CertFile, "cert", "", "TLS certificate file")
	serverCmd.Flags().StringVar(&KeyFile, "key", "", "TLS key file")
	rootCmd.AddCommand(serverCmd)
}
