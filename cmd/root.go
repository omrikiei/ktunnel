package cmd

import (
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

const (
	version = "1.2.7"
)

var Port int
var Tls bool
var Verbose bool
var DialTimeout int
var Host string
var CaFile string
var Scheme string
var ServerHostOverride string

var rootCmd = &cobra.Command{
	Use:     "ktunnel",
	Short:   "Ktunnel is a network tunneling tool for kubernetes",
	Long:    `Built to ease development on kubernetes clusters and allow connectivity between dev machines and clusters`,
	Version: version,
	Args:    cobra.MinimumNArgs(1),
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		log.SetFormatter(&log.TextFormatter{
			ForceColors:     true,
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02 15:04:05",
		})

		if Verbose {
			log.SetLevel(log.DebugLevel)
		}
	},
}

func Execute() {
	if genDoc := os.Getenv("GEN_DOC"); genDoc == "true" {
		err := doc.GenMarkdownTree(rootCmd, "./docs")
		if err != nil {
			log.Errorf("Failed generating docs: %v", err)
		}
	}

	if err := rootCmd.Execute(); err != nil {
		log.Error(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().IntVarP(&Port, "port", "p", 28688, "The port to use to establish the tunnel")
	rootCmd.PersistentFlags().BoolVarP(&Tls, "tls", "t", false, "Connection uses TLS if true, else plain TCP")
	rootCmd.PersistentFlags().BoolVarP(&Verbose, "verbose", "v", false, "Emit debug logs")
	rootCmd.PersistentFlags().IntVarP(&DialTimeout, "dial-timeout", "d", 500, "Local dial timeout in milliseconds")
	rootCmd.Flags().StringVarP(&CaFile, "ca-file", "c", "", "TLS cert auth file")
	rootCmd.Flags().StringVarP(&Scheme, "scheme", "s", "tcp", "Connection scheme")
	rootCmd.Flags().StringVarP(&ServerHostOverride, "server-host-override", "o", "", "Server name use to verify the hostname returned by the TLS handshake")
	rootCmd.Flags().StringVarP(&Namespace, "namespace", "n", "default", "Namespace")

	_ = rootCmd.MarkFlagRequired("port")
}
