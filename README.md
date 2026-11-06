# OpenDM — open-source download manager

An IDM-style download manager for Windows: one small `.exe`, no installer, and no dependencies outside the Go standard library. MIT licensed.

## Features

- **Multi-connection downloads.** Up to 32 parallel connections per file (8 by default).
- **Dynamic segmentation.** When a connection finishes its part, it splits the largest remaining part and takes half, the same trick that makes IDM fast. Slow connections never hold a download back.
- **Pause and resume**, including after closing OpenDM or rebooting. Progress is saved continuously.
- **Auto-retry.** Dropped or stalled connections reconnect with backoff and continue from the exact byte where they stopped. Every server reply is checked against the requested byte range.
- **Refresh address.** If a link expires, paste a fresh one and the download continues.
- **Queue.** You choose how many downloads run at once (3 by default). The rest wait their turn.
- **Speed limiter.** Sets a global cap shared by all downloads.
- **Category folders.** Files are sorted into Video, Music, Documents, Compressed, Programs, Images and General, like IDM.
- **Browser integration** for Edge, Chrome and Brave:
  - Downloads are caught automatically, with cookies and referer, so links that need a login still work.
  - Right-click a link and choose **Download with OpenDM**.
- **Tray icon:**
  - Closing the window keeps downloads running.
  - Right-click for Resume all, Pause all or Exit.
  - A notification appears when a file finishes.
- **Batch add.** Paste many links at once, press <kbd>Ctrl</kbd>+<kbd>V</kbd> anywhere in the window, or drag a link onto it.
- **Start with Windows**, minimized to the tray. **Proxy** support for office networks.
- Detects file names from `Content-Disposition`, including UTF-8 names, and never overwrites an existing file. A duplicate is saved as `name (1).ext`.

## Install

1. Unzip `OpenDM-windows-x64.zip` anywhere, for example `C:\Tools\OpenDM`.
2. Double-click `OpenDM.exe`. The window opens and an icon appears in the tray.
   - Windows SmartScreen may warn the first time because the exe is not code-signed. Click **More info**, then **Run anyway**. You can build it yourself from source (see below) if you prefer.
3. *(Optional)* Open **Settings**, then **General**, and tick **Start OpenDM when Windows starts**.

### Browser extension

1. Open OpenDM and go to **Settings**, then **Browser integration**, then **Unpack extension folder**. The `browser-extension` folder in the zip works too.
2. Go to `edge://extensions` or `chrome://extensions` and turn on **Developer mode**.
3. Click **Load unpacked** and select that folder.
4. Pin the OpenDM icon to the toolbar. The popup lets you:
   - turn capturing on or off
   - skip small files
   - exclude file types such as `jpg` or `png`

If OpenDM isn't running, the browser downloads the file as usual. Nothing is lost.

## Where things are stored

| What | Where |
|---|---|
| Downloads | `%USERPROFILE%\Downloads\<Category>\` (change it in Settings) |
| Partial files | next to the final file, as `*.opendm-part` |
| Settings, queue, log | `%APPDATA%\OpenDM\` |

## How it works

OpenDM runs a small local server on `127.0.0.1:6801`. Only your own machine can reach it. Requests coming from websites are rejected, and only the OpenDM window and the browser extension are allowed to send commands. The window is a local web page shown in an Edge or Chrome app window, so it looks like a normal desktop app.

The engine first checks whether the server supports byte ranges. If it does, OpenDM reserves the full file size on disk and downloads the ranges in parallel, writing each one directly at its offset. Finished parts keep splitting the slowest remaining part until the file is complete. Servers without range support fall back to a single connection automatically.

## Build from source

Requires Go 1.22 or newer. There are no third-party modules.

```bat
build.bat
```

Or on any OS:

```sh
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -s -w" -o OpenDM.exe .
go test ./...        # engine tests: multi-connection, pause/resume across restart, flaky servers, ...
```

The `rsrc_windows_*.syso` files hold the icon and manifest. Go embeds them automatically. To regenerate the icons, run `go run ./tools/genicons`.

OpenDM also runs on Linux and macOS (`go run .`). The browser window and download engine work the same, but the tray icon and folder picker are Windows-only.

## Licence

MIT. See `LICENSE`.
