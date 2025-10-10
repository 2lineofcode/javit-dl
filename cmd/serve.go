package cmd

import (
	"javit_dl/internal/globals"
	"javit_dl/internal/server"
	"log"

	"github.com/spf13/cobra"
)

var port string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run web server",
	Run: func(cmd *cobra.Command, args []string) {
		deps := server.Deps{HTTPClient: globals.Client, UserAgent: globals.UserAgent}
		if err := server.Start(port, deps); err != nil { log.Fatal(err) }
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.PersistentFlags().StringVarP(&port, "port", "p", ":8080", "Server port")
}
