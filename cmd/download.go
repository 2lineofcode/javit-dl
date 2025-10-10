package cmd

import (
	"fmt"
	"javit_dl/internal/downloader"
	"javit_dl/internal/globals"
	"log"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download via CLI pipeline",
	Run: func(cmd *cobra.Command, args []string) {
		if flagURL == "" {
			_ = cmd.Help()
			os.Exit(1)
		}
		if flagOut == "" {
			fmt.Println("Output file cannot be empty.")
			os.Exit(1)
		}
		if flagOut != "" && len(flagOut) > 4 && flagOut[len(flagOut)-4:] == ".mp4" {
			fmt.Println("Output file should not have .mp4 extension, it will be added automatically.")
			os.Exit(1)
		}
		start := time.Now()
		fmt.Println("Starting download:", flagURL)
		finalMP4, err := downloader.RunDownloadForCLI(globals.Client, globals.UserAgent, flagURL, flagOut, flagConcurrency, flagRetries, flagTmpdir, flagAuto, flagRaw)
		if err != nil { log.Fatal(err) }
		fmt.Println("✅ done →", finalMP4)
		if _, err := os.Stat(flagOut); err == nil { _ = os.Remove(flagOut) }
		fmt.Println("All done in", time.Since(start).Round(time.Second))
	},
}

func init() {
	rootCmd.AddCommand(downloadCmd)
}
