package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	appName    = "OpenDM"
	appVersion = "1.0.0"
	partSuffix = ".opendm-part"
)

type Config struct {
	DownloadDir      string `json:"downloadDir"`
	SortByCategory   bool   `json:"sortByCategory"`
	Connections      int    `json:"connections"`
	MaxConcurrent    int    `json:"maxConcurrent"`
	SpeedLimitKB     int    `json:"speedLimitKB"`
	Retries          int    `json:"retries"`
	Port             int    `json:"port"`
	OpenAppWindow    bool   `json:"openAppWindow"`
	StartWithWindows bool   `json:"startWithWindows"`
	NotifyOnComplete bool   `json:"notifyOnComplete"`
	UserAgent        string `json:"userAgent"`
	Proxy            string `json:"proxy"`
}

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DownloadDir:      filepath.Join(home, "Downloads"),
		SortByCategory:   true,
		Connections:      8,
		MaxConcurrent:    3,
		Retries:          6,
		Port:             6801,
		OpenAppWindow:    true,
		NotifyOnComplete: true,
		UserAgent:        defaultUA,
	}
}

func (c *Config) normalize() {
	d := defaultConfig()
	if c.DownloadDir == "" {
		c.DownloadDir = d.DownloadDir
	}
	c.Connections = clamp(c.Connections, 1, 32)
	c.MaxConcurrent = clamp(c.MaxConcurrent, 1, 20)
	c.Retries = clamp(c.Retries, 0, 50)
	if c.SpeedLimitKB < 0 {
		c.SpeedLimitKB = 0
	}
	if c.Port < 1024 || c.Port > 65535 {
		c.Port = d.Port
	}
	if c.UserAgent == "" {
		c.UserAgent = defaultUA
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func appDataDir() string {
	if d := os.Getenv("OPENDM_HOME"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, appName)
}

func loadConfig(dir string) Config {
	c := defaultConfig()
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	c.normalize()
	return c
}

func saveConfig(dir string, c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(filepath.Join(dir, "settings.json"), b)
}

func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
