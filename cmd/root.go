package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "go-chia-crawler",
	Short: "Chia peer crawler",
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().String("bootstrap-peer", "node.chia.net", "The initial bootstrap peer to try and connect to")
	rootCmd.PersistentFlags().Bool("metrics", false, "Enable the metrics server")
	rootCmd.PersistentFlags().Int("metrics-port", 9914, "The port the metrics server binds to")
	rootCmd.PersistentFlags().String("data-dir", "", "The directory to store crawler data in")

	cobra.CheckErr(viper.BindPFlag("bootstrap-peer", rootCmd.PersistentFlags().Lookup("bootstrap-peer")))
	cobra.CheckErr(viper.BindPFlag("metrics", rootCmd.PersistentFlags().Lookup("metrics")))
	cobra.CheckErr(viper.BindPFlag("metrics-port", rootCmd.PersistentFlags().Lookup("metrics-port")))
	cobra.CheckErr(viper.BindPFlag("data-dir", rootCmd.PersistentFlags().Lookup("data-dir")))

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.chia-crawler.yaml)")
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)

		// Search config in home directory with name ".go-chia-crawler" (without extension).
		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName(".chia-crawler")
	}

	viper.SetEnvPrefix("CHIA_CRAWLER")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		fmt.Fprintln(os.Stderr, "Using config file:", viper.ConfigFileUsed())
	}
}
