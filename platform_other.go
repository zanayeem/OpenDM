//go:build !windows

package main

import (
	"errors"
	"log"
	"os/exec"
	"path/filepath"
	"runtime"
)

func platformName() string { return runtime.GOOS }

func opener() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

func openPath(p string) error { return exec.Command(opener(), p).Start() }

func revealFile(p string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", "-R", p).Start()
	}
	return openPath(filepath.Dir(p))
}

func openUI(u string, appWindow bool) {
	if err := openPath(u); err != nil {
		log.Printf("open browser: %v", err)
	}
}

func pickFolder(start string) (string, error) {
	return "", errors.New("folder picker is only available on Windows; type the path instead")
}

func setAutostart(enable bool) {}

func fatalBox(msg string) { log.Println(msg) }

func runTray(a trayActions) bool   { return false }
func trayNotify(title, msg string) {}
func trayClose()                   {}
func traySetTip(tip string)        {}
func allowForeground()             {}
