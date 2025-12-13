/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	_const "github.com/janghanul090801/pigo/cmd/const"
	"github.com/spf13/cobra"
)

// installCmd represents the install command
var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install package",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		installArgs := append([]string{"install"}, args...)
		installCmd := exec.Command(_const.PIPPATHWINDOW, installArgs...)
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		installCmd.Stdin = os.Stdin

		if err := installCmd.Run(); err != nil {
			log.Fatalf("error: %v", err)
		}

		var targetPackages []string
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				targetPackages = append(targetPackages, arg)
			}
		}

		if len(targetPackages) == 0 {
			return
		}

		showArgs := append([]string{"show"}, targetPackages...)
		showCmd := exec.Command(_const.PIPPATHWINDOW, showArgs...)

		var out bytes.Buffer
		showCmd.Stdout = &out
		showCmd.Stderr = os.Stderr

		if err := showCmd.Run(); err != nil {
			log.Printf("warning: failed to get package info for requirements.txt: %v", err)
		}

		file, err := os.OpenFile("requirements.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("error creating file: %v", err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(&out)
		var currentName, currentVersion string

		for scanner.Scan() {
			line := scanner.Text()

			if strings.HasPrefix(line, "Name: ") {
				currentName = strings.TrimSpace(strings.TrimPrefix(line, "Name: "))
			} else if strings.HasPrefix(line, "Version: ") {
				currentVersion = strings.TrimSpace(strings.TrimPrefix(line, "Version: "))

				if currentName != "" && currentVersion != "" {
					_, err := file.WriteString(fmt.Sprintf("%s==%s\n", currentName, currentVersion))
					if err != nil {
						log.Printf("error writing to file: %v", err)
					}
					currentName = ""
					currentVersion = ""
				}
			}
		}
	},
}

func init() {
	rootCmd.AddCommand(installCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// installCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// installCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
