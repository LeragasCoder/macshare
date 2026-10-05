# Droply

Local file transfer between a Mac and a phone. The Mac runs a small server and a
control panel; the phone opens a web page (scan the QR code). No app is installed
on the phone, and file bytes stay on the local network.

The phone and the Mac must be on the same network: shared Wi-Fi, or the phone's
hotspot with the Mac joined to it.

## نصب و اجرا

پیش‌نیاز: macOS و Go 1.21 یا جدیدتر.

```bash
brew install go          # فقط یک بار
cd macshare
go mod tidy
go run .
```

پنل مک خودش در مرورگر باز می‌شود (`http://127.0.0.1:8081`). QR را با گوشی اسکن کن،
یا آدرس را کپی کن. اگر macOS پرسید اجازهٔ اتصال ورودی بده (Firewall).

ساخت باینری تکی (HTML داخل باینری embed شده؛ لازم نیست پوشهٔ `web/` کنار باینری باشد):

```bash
go build -o macshare
./macshare                         # فقط پنل
./macshare ~/Desktop/movie.mp4     # همان لحظه فایل را برای گوشی بگذار
```

فایل‌هایی که گوشی می‌فرستد پیش‌فرض می‌روند توی `~/Downloads/MacShare`.

## Install and run

Requires macOS and Go 1.21 or newer (`brew install go`, or the installer from
[go.dev/dl](https://go.dev/dl/)).

```bash
git clone https://github.com/LeragasCoder/macshare.git
cd macshare
go mod tidy
go run .
```

Or build a single binary. The pages are embedded with `go:embed`, so the binary
does not need the `web/` folder next to it:

```bash
go build -o macshare
./macshare                         # open the control panel
./macshare ~/Desktop/movie.mp4     # share these paths immediately
```

macOS may prompt to allow incoming connections the first time. Allow it, or the
phone cannot reach the Mac. The control panel itself listens only on
`127.0.0.1`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-port` | `8080` | Port the phone connects to (`0.0.0.0`) |
| `-admin-port` | `8081` | Control panel port (`127.0.0.1` only) |
| `-dir PATH` | `~/Downloads/MacShare` | Folder for files received from the phone |
| `-no-open` | off | Do not open the control panel in the browser |

Example:

```bash
./macshare -port 9090 -dir ~/Desktop/FromPhone -no-open ~/Movies/clip.mov
```

Stop the process with Ctrl+C. While it is running, `caffeinate` keeps the Mac
from idle-sleeping so a long transfer is not frozen mid-file.

## How to use it

1. Start MacShare. The panel opens at `http://127.0.0.1:8081`.
2. Add files or a folder. Folders are streamed as an uncompressed zip (dotfiles are skipped).
3. On the phone, scan the QR code or open the copied URL. Pick the right interface if the Mac has more than one (Wi-Fi vs. Ethernet vs. hotspot).
4. Save files in the phone browser. Downloads use HTTP Range, so an interrupted file can be resumed by the browser.
5. To send the other way, leave **Accept uploads from phone** on and pick files on the phone. Received names are sanitized and will not overwrite an existing file (`name (1).ext`).

Turn uploads off to make the Mac send-only. **New access key** invalidates every
phone that still has the old link.

## What the panel shows

- Live throughput graph (last 60 seconds) and per-transfer speed, peak, and ETA, both directions
- Transfer history with average speed, peak, duration, and completed vs. interrupted
- Phone-side speed test: latency, jitter, download, upload, 1 to 8 streams. The test does not write files
- Configurable save folder, with **Show in Finder**

Speed can be shown as MB/s or Mbit/s. The choice is stored in the browser.

## How access works

- The control panel binds to `127.0.0.1` only. State-changing panel routes accept `POST` only and require the `X-Req: 1` header the panel sends, so a random page cannot call them with a GET.
- The phone site is on the LAN port and is gated by a random access key in the path (`/a1b2c3d4e5f6/`). Regenerating the key closes old links. The key is 12 hex characters; treat the URL like a password on a shared network.
- Anyone with that URL can list and download whatever is currently shared, and can upload if uploads are enabled. There is no per-file size cap.
- Nothing is uploaded to a cloud service. The QR code is generated on the Mac.

File picker, Finder reveal, and `caffeinate` use macOS commands (`osascript`, `open`). The transfer server itself is plain Go.

## Layout

```
main.go           HTTP servers, sharing, uploads, speed-test endpoints
transfers.go      byte counters, live speed, history
web/admin.html    control panel (embedded)
web/phone.html    phone page (embedded)
```

No license file is included; all rights reserved unless you add one.
