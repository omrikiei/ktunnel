package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"os"
)

const (
	version = "1.0.0-alpha"
)

var Port int
var Tls bool

var rootCmd = &cobra.Command{
	Use:   "ktunnel",
	Short: "Ktunnel is a network tunneling tool for kubernetes",
	Long: `Built to ease development on kubernetes clusters and allow connectivity between dev machines and clusters`,
	Version: version,
	Args: cobra.MinimumNArgs(1),
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().IntVarP(&Port, "port", "p", 28688, "author name for copyright attribution")
	rootCmd.PersistentFlags().BoolVarP(&Tls, "tls", "t", false, "Connection uses TLS if true, else plain TCP")
	_ = rootCmd.MarkFlagRequired("port")
}

