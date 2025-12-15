/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package main

import (
	"github.com/janghanul090801/pigo/cmd"
	_const "github.com/janghanul090801/pigo/cmd/const"
	"runtime"
)

func main() {
	switch runtime.GOOS {
	case "windows":
		_const.PIPPATH = _const.PIPPATHWINDOW
		_const.PYTHONPATH = _const.PYTHONPATHWINDOW
	case "linux":
		_const.PIPPATH = _const.PIPPATHLINUX
		_const.PYTHONPATH = _const.PYTHONPATHLINUX
	}
	cmd.Execute()
}
