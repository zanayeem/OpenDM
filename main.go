// OpenDM — an open-source, IDM-style download manager.
// MIT licensed. Pure Go standard library, no third-party dependencies.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func itoa(i int) string { return strconv.Itoa(i) }

func main() {
	background := flag.Bool("background", false, "start minimised to the tray without opening the window")
	flag.Parse()

	dataDir := appDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fatalBox("Cannot create data folder: " + err.Error())
		return
	}
	if lf, err := os.OpenFile(filepath.Join(dataDir, "opendm.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(lf, os.Stderr))
	}

	cfg := loadConfig(dataDir)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		if pingExisting(cfg.Port) {
			// already running: just bring up the window
			if !*background {
				allowForeground()
				askExistingToShow(cfg.Port)
			}
			return
		}
		fatalBox(fmt.Sprintf("Port %d is used by another program.\nChange \"port\" in %s",
			cfg.Port, filepath.Join(dataDir, "settings.json")))
		return
	}
	_ = saveConfig(dataDir, cfg)

	m := NewManager(cfg, dataDir)
	m.notify = trayNotify
	m.Load()

	uiURL := fmt.Sprintf("http://127.0.0.1:%d/", cfg.Port)
	show := func() { openUI(uiURL, m.cfg().OpenAppWindow) }

	var quitOnce sync.Once
	quit := func() {
		quitOnce.Do(func() {
			log.Println("shutting down")
			m.Shutdown()
			trayClose()
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		})
	}

	srv := &Server{m: m, port: cfg.Port, onQuit: quit, onShow: show}
	go func() {
		hs := &http.Server{Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second}
		if err := hs.Serve(ln); err != nil {
			log.Printf("http server: %v", err)
		}
	}()
	log.Printf("%s %s listening on %s (data: %s)", appName, appVersion, uiURL, dataDir)

	if !*background {
		show()
	}

	// keep the tray tooltip showing live progress
	go func() {
		for range time.Tick(2 * time.Second) {
			n, speed := 0, 0.0
			for _, v := range m.Views() {
				if v.Status == StDownloading {
					n++
					speed += v.Speed
				}
			}
			tip := appName + " — idle"
			if n > 0 {
				tip = fmt.Sprintf("%s — %d downloading · %s/s", appName, n, humanBytes(int64(speed)))
			}
			traySetTip(tip)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; quit() }()

	// On Windows this runs the tray icon's message loop and blocks.
	if !runTray(trayActions{
		Open:      show,
		PauseAll:  m.PauseAll,
		ResumeAll: m.ResumeAll,
		Quit:      quit,
	}) {
		fmt.Printf("%s is running at %s — press Ctrl+C to quit\n", appName, uiURL)
		select {}
	}
	quit()
}

type trayActions struct {
	Open, PauseAll, ResumeAll, Quit func()
}
