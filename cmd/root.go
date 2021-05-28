package cmd

import (
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

const (
	version = "1.4.0"
)

var port int
var tls bool
var verbose bool

var rootCmd = &cobra.Command{
	Use:     "ktunnel",
	Short:   "Ktunnel is a network tunneling tool for kubernetes",
	Long:    `Built to ease development on kubernetes clusters and allow connectivity between dev machines and clusters`,
	Version: version,
	Args:    cobra.MinimumNArgs(1),
}

var logger = log.Logger{
	Out: os.Stdout,
	Formatter: &log.TextFormatter{
		ForceColors:     true,
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05.000",
	},
	Level: log.InfoLevel,
}

func Execute() {
	if genDoc := os.Getenv("GEN_DOC"); genDoc == "true" {
		err := doc.GenMarkdownTree(rootCmd, "./docs")
		if err != nil {
			log.Errorf("Failed generating docs: %v", err)
		}
	}

	if err := rootCmd.Execute(); err != nil {
		logger.WithError(err).Errorf("error executing command")
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().IntVarP(&port, "port", "p", 28688, "The port to use to establish the tunnel")
	rootCmd.PersistentFlags().BoolVarP(&tls, "tls", "t", false, "Connection uses tls if true, else plain TCP")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose mode")
	_ = rootCmd.MarkFlagRequired("port")
}
