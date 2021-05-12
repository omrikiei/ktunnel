package cmd
/*
import (
	"context"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var interactiveCmd = &cobra.Command{
	Use:   "interactive [flags] [ports]",
	Short: "Run ktunnel interactively",
	Long:  `This command would open a terminal UI`,
	Args:  cobra.MinimumNArgs(1),
	Example: `
	`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		if verbose {
			logger.SetLevel(log.DebugLevel)
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


	},
}
*/