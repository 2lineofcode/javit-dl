package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	flagURL         string
	flagOut         string
	flagConcurrency int
	flagRetries     int
	flagTmpdir      string
	flagAuto        bool
	flagRaw         bool
)

var rootCmd = &cobra.Command{
	Use:   "javit_dl",
	Short: "M3U8 & File Downloader",
	Long:  "Fast M3U8 (HLS) and file downloader with optional decryption and MP4 conversion.",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagURL, "url", "u", "", "URL to m3u8 playlist or raw file")
	rootCmd.PersistentFlags().StringVarP(&flagOut, "out", "o", "output.ts", "Output file")
	rootCmd.PersistentFlags().IntVarP(&flagConcurrency, "concurrency", "c", 8, "Concurrent downloads (M3U8 only)")
	rootCmd.PersistentFlags().IntVarP(&flagRetries, "retries", "r", 3, "Retries per segment (M3U8 only)")
	rootCmd.PersistentFlags().StringVarP(&flagTmpdir, "temp", "t", "m3u8_tmp", "Temporary segment directory (M3U8 only)")
	rootCmd.PersistentFlags().BoolVar(&flagAuto, "auto", false, "Auto select highest bandwidth variant (M3U8 only)")
	rootCmd.PersistentFlags().BoolVar(&flagRaw, "rawurl", false, "Just download the single file URL, skip M3U8 parsing.")
}
