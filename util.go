package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Category folders, modelled after the classic IDM layout.
var categoryExt = map[string][]string{
	"Video":      {"mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "mpg", "mpeg", "3gp", "ts", "m2ts", "vob"},
	"Music":      {"mp3", "wav", "flac", "aac", "ogg", "m4a", "wma", "opus", "aiff", "mid"},
	"Compressed": {"zip", "rar", "7z", "tar", "gz", "bz2", "xz", "tgz", "iso", "cab", "zst", "img"},
	"Documents":  {"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "rtf", "odt", "ods", "odp", "csv", "epub", "md", "json", "xml"},
	"Programs":   {"exe", "msi", "msix", "msixbundle", "appx", "apk", "dmg", "pkg", "deb", "rpm", "bat", "jar", "appimage"},
	"Images":     {"jpg", "jpeg", "png", "gif", "bmp", "webp", "svg", "tiff", "ico", "heic", "psd", "avif"},
}

var extToCategory = func() map[string]string {
	m := map[string]string{}
	for cat, exts := range categoryExt {
		for _, e := range exts {
			m[e] = cat
		}
	}
	return m
}()

func categorize(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if c, ok := extToCategory[ext]; ok {
		return c
	}
	return "General"
}

var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeFileName makes a name safe for Windows (and every other OS).
func sanitizeFileName(name string) string {
	name = strings.ToValidUTF8(name, "_")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || r == 127:
			continue
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	if out == "" {
		return "download"
	}
	stem := strings.TrimSuffix(out, filepath.Ext(out))
	if reservedNames[strings.ToUpper(stem)] {
		out = "_" + out
	}
	// keep names well under MAX_PATH while preserving the extension
	if utf8.RuneCountInString(out) > 180 {
		ext := filepath.Ext(out)
		if utf8.RuneCountInString(ext) > 20 {
			ext = ""
		}
		r := []rune(strings.TrimSuffix(out, ext))
		out = string(r[:180-utf8.RuneCountInString(ext)]) + ext
	}
	return out
}

// uniquePath returns dir/name, or "name (n).ext" if something already exists there.
func uniquePath(dir, name string, taken func(string) bool) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		p := filepath.Join(dir, candidate)
		if exists(p) || exists(p+partSuffix) || (taken != nil && taken(p)) {
			continue
		}
		return p
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var commonMimeExt = map[string]string{
	"application/pdf":              ".pdf",
	"application/zip":              ".zip",
	"application/x-zip-compressed": ".zip",
	"application/x-msdownload":     ".exe",
	"application/x-rar-compressed": ".rar",
	"application/vnd.rar":          ".rar",
	"application/x-7z-compressed":  ".7z",
	"application/gzip":             ".gz",
	"video/mp4":                    ".mp4",
	"video/webm":                   ".webm",
	"video/x-matroska":             ".mkv",
	"audio/mpeg":                   ".mp3",
	"audio/mp4":                    ".m4a",
	"image/jpeg":                   ".jpg",
	"image/png":                    ".png",
	"image/gif":                    ".gif",
	"image/webp":                   ".webp",
	"text/plain":                   ".txt",
	"application/json":             ".json",
	"application/x-iso9660-image":  ".iso",
}

// fileNameFromResponse picks the best file name using Content-Disposition,
// then the (post-redirect) URL path, then the content type.
func fileNameFromResponse(resp *http.Response, rawURL string) string {
	name := ""
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			name = params["filename"] // ParseMediaType already decodes filename*
		}
		if name == "" {
			// tolerate sloppy headers like: attachment; filename=foo bar.zip
			if i := strings.Index(strings.ToLower(cd), "filename="); i >= 0 {
				name = strings.Trim(strings.TrimSpace(strings.SplitN(cd[i+9:], ";", 2)[0]), `"'`)
			}
		}
	}
	if name == "" {
		u := resp.Request.URL
		if u == nil {
			u, _ = url.Parse(rawURL)
		}
		if u != nil {
			name = nameFromURL(u)
		}
	}
	name = sanitizeFileName(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if filepath.Ext(name) == "" {
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if ext, ok := commonMimeExt[ct]; ok {
			name += ext
		} else if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 && ct != "application/octet-stream" {
			name += exts[0]
		}
	}
	return name
}

func nameFromURL(u *url.URL) string {
	base := path.Base(u.Path)
	if base == "/" || base == "." || base == "" {
		return u.Hostname()
	}
	if un, err := url.PathUnescape(base); err == nil {
		base = un
	}
	return base
}

func guessNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "download"
	}
	return sanitizeFileName(nameFromURL(u))
}
