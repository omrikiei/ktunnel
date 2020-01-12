package cmd

import (
	"fmt"
	"ktunnel/pkg/k8s"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

const (
	version = "1.1.4"
)

var Port int
var Tls bool
var Verbose bool

var rootCmd = &cobra.Command{
	Use:     "ktunnel",
	Short:   "Ktunnel is a network tunneling tool for kubernetes",
	Long:    `Built to ease development on kubernetes clusters and allow connectivity between dev machines and clusters`,
	Version: version,
	Args:    cobra.MinimumNArgs(1),
}

func Execute() {
	if genDoc := os.Getenv("GEN_DOC"); genDoc == "true" {
		err := doc.GenMarkdownTree(rootCmd, "./docs")
		if err != nil {
			log.Errorf("Failed generating docs: %v", err)
		}
	}
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().IntVarP(&Port, "port", "p", 28688, "The port to use to establish the tunnel")
	rootCmd.PersistentFlags().BoolVarP(&Tls, "tls", "t", false, "Connection uses TLS if true, else plain TCP")
	rootCmd.PersistentFlags().BoolVarP(&Verbose, "verbose", "v", false, "Emit debug logs")
	_ = rootCmd.MarkFlagRequired("port")
	if Verbose == true {
		log.SetLevel(log.DebugLevel)
		k8s.Verbose = true
	}
}
