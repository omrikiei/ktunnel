package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of Ktunnel",
	Long:  `All software has versions. This is Ktunnel's`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version)
	},
}
