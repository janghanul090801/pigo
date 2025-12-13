/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	_const "github.com/janghanul090801/pigo/cmd/const"
	"github.com/spf13/cobra"
)

// uninstallCmd represents the uninstall command
var uninstallCmd = &cobra.Command{
	Use:                "uninstall",
	Short:              "Uninstall package and remove from requirements.txt",
	Long:               `Uninstall a package using pip and remove it from the requirements.txt file.`,
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		uninstallArgs := append([]string{"uninstall"}, args...)
		uninstallCmd := exec.Command(_const.PIPPATHWINDOW, uninstallArgs...)
		uninstallCmd.Stdout = os.Stdout
		uninstallCmd.Stderr = os.Stderr
		uninstallCmd.Stdin = os.Stdin

		if err := uninstallCmd.Run(); err != nil {
			log.Fatalf("error executing pip uninstall: %v", err)
		}

		targetPackages := make(map[string]bool)
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				targetPackages[strings.ToLower(arg)] = true
			}
		}

		if len(targetPackages) == 0 {
			return
		}

		file, err := os.Open("requirements.txt")
		if os.IsNotExist(err) {
			return
		} else if err != nil {
			log.Fatalf("error opening requirements.txt: %v", err)
		}
		defer file.Close()

		var newLines []string
		scanner := bufio.NewScanner(file)

		for scanner.Scan() {
			line := scanner.Text()
			trimmedLine := strings.TrimSpace(line)

			if trimmedLine == "" {
				continue
			}

			if strings.HasPrefix(trimmedLine, "#") {
				newLines = append(newLines, line)
				continue
			}

			delimiters := "=<>!~@;["

			idx := strings.IndexAny(trimmedLine, delimiters)

			var pkgNameInFile string
			if idx == -1 {
				pkgNameInFile = trimmedLine
			} else {
				pkgNameInFile = trimmedLine[:idx]
			}

			pkgNameInFile = strings.ToLower(strings.TrimSpace(pkgNameInFile))

			if _, found := targetPackages[pkgNameInFile]; found {
				fmt.Printf("Removing %s from requirements.txt\n", pkgNameInFile)
				continue
			}

			newLines = append(newLines, line)
		}

		if err := scanner.Err(); err != nil {
			log.Fatalf("error reading requirements.txt: %v", err)
		}
		file.Close()

		outFile, err := os.Create("requirements.txt")
		if err != nil {
			log.Fatalf("error writing requirements.txt: %v", err)
		}
		defer outFile.Close()

		w := bufio.NewWriter(outFile)
		for _, line := range newLines {
			fmt.Fprintln(w, line)
		}
		w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(uninstallCmd)
}
