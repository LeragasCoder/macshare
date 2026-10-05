package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/admin.html
var adminHTML []byte

//go:embed web/phone.html
var phoneHTML []byte

const (
	bufSize = 1 << 20 // 1 MB copy buffer
	sockBuf = 2 << 20 // 2 MB socket buffers
)

var (
	publicPort = 8080
	adminPort  = 8081
)

type ctxKey struct{}

// ---------- state ----------

type Item struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"-"`
	Size      int64  `json:"size"`
	IsDir     bool   `json:"isDir"`
	Downloads int64  `json:"downloads"`
}

type TestReport struct {
	At      string  `json:"at"`
	Peer    string  `json:"peer"`
	PingMs  float64 `json:"pingMs"`
	Jitter  float64 `json:"jitterMs"`
	Down    float64 `json:"down"` // bytes/s
	Up      float64 `json:"up"`   // bytes/s
	PeakDn  float64 `json:"peakDown"`
	PeakUp  float64 `json:"peakUp"`
	Streams int     `json:"streams"`
}

type App struct {
	mu      sync.RWMutex
	items   []*Item
	nextID  int
	token   string
	recvDir string
	tests   []TestReport
	uploads atomic.Bool
	tm      *TransferMgr
}

func (a *App) add(path string) {
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	it := &Item{Name: filepath.Base(path), Path: path, IsDir: st.IsDir(), Size: st.Size()}
	if st.IsDir() {
		var total int64
		filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") && p != path {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				if info, e := d.Info(); e == nil && info.Mode().IsRegular() {
					total += info.Size()
				}
			}
			return nil
		})
		it.Size = total
	}
	a.mu.Lock()
	a.nextID++
	it.ID = strconv.Itoa(a.nextID)
	a.items = append(a.items, it)
	a.mu.Unlock()
}

func (a *App) find(id string) *Item {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, it := range a.items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

func (a *App) remove(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, it := range a.items {
		if it.ID == id {
			a.items = append(a.items[:i], a.items[i+1:]...)
			return
		}
	}
}

func (a *App) snapshotItems() []Item {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Item, len(a.items))
	for i, it := range a.items {
		out[i] = *it
		out[i].Downloads = atomic.LoadInt64(&it.Downloads)
	}
	return out
}

func (a *App) getToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.token
}

func (a *App) getRecvDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.recvDir
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func randomToken() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type IPInfo struct {
	Iface string `json:"iface"`
	IP    string `json:"ip"`
}

func localIPs() []IPInfo {
	var out []IPInfo
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			ipn, ok := ad.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, IPInfo{Iface: ifc.Name, IP: ip.String()})
		}
	}
	return out
}

func peerOf(r *http.Request) string {
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

func connOf(r *http.Request) *countConn {
	c, _ := r.Context().Value(ctxKey{}).(*countConn)
	return c
}

func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

func safeName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if r < 32 || r == '/' || r == ':' {
			return '_'
		}
		return r
	}, n)
	if n == "" || n == "." || n == ".." {
		n = "file"
	}
	return n
}

func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
	}
}

// ---------- public server (phone) ----------

func (a *App) publicHandler() http.Handler {
	sub := http.NewServeMux()

	sub.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(phoneHTML)
	})

	sub.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.snapshotItems())
	})

	sub.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		snap := a.tm.snapshot()
		files := []Transfer{}
		for _, t := range snap.Active {
			if t.Kind == "file" {
				files = append(files, t)
			}
		}
		host, _ := os.Hostname()
		writeJSON(w, map[string]any{"host": host, "uploads": a.uploads.Load(), "active": files})
	})

	sub.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})

	sub.HandleFunc("/d/", a.handleDownload)
	sub.HandleFunc("/up", a.handleUpload)
	sub.HandleFunc("/zero", a.handleZero)
	sub.HandleFunc("/sink", a.handleSink)

	sub.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var rep TestReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&rep); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		rep.At = time.Now().Format("15:04:05")
		rep.Peer = peerOf(r)
		a.mu.Lock()
		a.tests = append([]TestReport{rep}, a.tests...)
		if len(a.tests) > 10 {
			a.tests = a.tests[:10]
		}
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	// The access key is part of the path and can be regenerated at runtime.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/" + a.getToken()
		switch {
		case r.URL.Path == prefix:
			http.Redirect(w, r, prefix+"/", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, prefix+"/"):
			http.StripPrefix(prefix, sub).ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	it := a.find(strings.TrimPrefix(r.URL.Path, "/d/"))
	cc := connOf(r)
	if it == nil || cc == nil {
		http.NotFound(w, r)
		return
	}
	atomic.AddInt64(&it.Downloads, 1)
	if it.IsDir {
		a.serveZip(w, r, it, cc)
		return
	}
	f, err := os.Open(it.Path)
	if err != nil {
		http.Error(w, "cannot open file", 500)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	size := st.Size()
	total := size
	if rg := r.Header.Get("Range"); rg != "" {
		var s, e int64
		n, _ := fmt.Sscanf(rg, "bytes=%d-%d", &s, &e)
		switch {
		case n == 2 && e >= s:
			total = min(e, size-1) - s + 1
		case n >= 1 && s < 0:
			total = min(-s, size)
		case n >= 1 && s < size:
			total = size - s
		}
	}
	base := cc.written.Load()
	t := a.tm.start("file", "send", it.Name, peerOf(r), total, func() int64 { return cc.written.Load() - base }, cc)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(it.Name))
	// ServeContent gives us Range support (resume) and sendfile(2).
	http.ServeContent(w, r, it.Name, st.ModTime(), f)
	a.tm.finish(t, t.src() >= total)
}

// Folders are streamed as an uncompressed zip.
func (a *App) serveZip(w http.ResponseWriter, r *http.Request, it *Item, cc *countConn) {
	base := cc.written.Load()
	t := a.tm.start("file", "send", it.Name+".zip", peerOf(r), it.Size, func() int64 { return cc.written.Load() - base }, cc)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(it.Name+".zip"))
	zw := zip.NewWriter(w)
	buf := make([]byte, bufSize)
	parent := filepath.Dir(it.Path)
	var werr error
	filepath.WalkDir(it.Path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") && p != it.Path {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(parent, p)
		h, _ := zip.FileInfoHeader(info)
		h.Name = filepath.ToSlash(rel)
		h.Method = zip.Store
		fw, e := zw.CreateHeader(h)
		if e != nil {
			werr = e
			return e
		}
		src, e := os.Open(p)
		if e != nil {
			return nil
		}
		defer src.Close()
		if _, e = io.CopyBuffer(fw, src, buf); e != nil {
			werr = e
			return e
		}
		return nil
	})
	if e := zw.Close(); e != nil && werr == nil {
		werr = e
	}
	a.tm.finish(t, werr == nil)
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", 405)
		return
	}
	if !a.uploads.Load() {
		http.Error(w, "uploads are disabled on the Mac", 403)
		return
	}
	cc := connOf(r)
	if cc == nil {
		http.Error(w, "no connection", 500)
		return
	}
	dir := a.getRecvDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	name := safeName(r.URL.Query().Get("name"))
	dst := uniquePath(dir, name)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var got atomic.Int64
	t := a.tm.start("file", "receive", filepath.Base(dst), peerOf(r), r.ContentLength, got.Load, cc)
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(tmp)
		}
		a.tm.finish(t, ok)
	}()
	buf := make([]byte, bufSize)
	for {
		n, rerr := r.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				http.Error(w, werr.Error(), 500)
				return
			}
			got.Add(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return
		}
	}
	f.Close()
	if err := os.Rename(tmp, dst); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ok = true
	w.WriteHeader(http.StatusNoContent)
}

// Network-only speed test: the server streams zeros (no disk involved).
func (a *App) handleZero(w http.ResponseWriter, r *http.Request) {
	cc := connOf(r)
	if cc == nil {
		http.Error(w, "no connection", 500)
		return
	}
	mb, _ := strconv.Atoi(r.URL.Query().Get("mb"))
	if mb <= 0 || mb > 8192 {
		mb = 512
	}
	base := cc.written.Load()
	t := a.tm.start("test", "send", "Speed test download", peerOf(r), int64(mb)<<20, func() int64 { return cc.written.Load() - base }, cc)
	defer a.tm.finish(t, true)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(int64(mb)<<20, 10))
	buf := make([]byte, bufSize)
	for i := 0; i < mb; i++ {
		if _, err := w.Write(buf); err != nil {
			return
		}
	}
}

func (a *App) handleSink(w http.ResponseWriter, r *http.Request) {
	cc := connOf(r)
	if cc == nil {
		http.Error(w, "no connection", 500)
		return
	}
	var got atomic.Int64
	t := a.tm.start("test", "receive", "Speed test upload", peerOf(r), r.ContentLength, got.Load, cc)
	defer a.tm.finish(t, true)
	buf := make([]byte, bufSize)
	for {
		n, err := r.Body.Read(buf)
		got.Add(int64(n))
		if err != nil {
			break
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- admin server (localhost only) ----------

func osascriptLines(script []string) []string {
	out, err := exec.Command("osascript", script...).Output()
	if err != nil { // user cancelled the dialog
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func pickPaths(kind string) []string {
	verb := "choose file"
	if kind == "folder" {
		verb = "choose folder"
	}
	return osascriptLines([]string{
		"-e", "set fs to " + verb + " with multiple selections allowed",
		"-e", `set out to ""`,
		"-e", "repeat with f in fs",
		"-e", "set out to out & POSIX path of f & linefeed",
		"-e", "end repeat",
		"-e", "return out",
	})
}

func (a *App) adminHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(adminHTML)
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		tests := append([]TestReport{}, a.tests...)
		a.mu.RUnlock()
		host, _ := os.Hostname()
		writeJSON(w, map[string]any{
			"host":    host,
			"ips":     localIPs(),
			"port":    publicPort,
			"token":   a.getToken(),
			"items":   a.snapshotItems(),
			"recvDir": a.getRecvDir(),
			"uploads": a.uploads.Load(),
			"tests":   tests,
			"stats":   a.tm.snapshot(),
		})
	})

	mux.HandleFunc("/qr.png", func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("u")
		if !strings.HasPrefix(u, "http://") {
			http.Error(w, "bad url", 400)
			return
		}
		png, err := qrcode.Encode(u, qrcode.Medium, 360)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})

	ok := func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

	mux.HandleFunc("/api/pick", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range pickPaths(r.URL.Query().Get("kind")) {
			a.add(p)
		}
		ok(w)
	})
	mux.HandleFunc("/api/remove", func(w http.ResponseWriter, r *http.Request) {
		a.remove(r.URL.Query().Get("id"))
		ok(w)
	})
	mux.HandleFunc("/api/clear", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.items = nil
		a.mu.Unlock()
		ok(w)
	})
	mux.HandleFunc("/api/cancel", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		a.tm.cancel(id)
		ok(w)
	})
	mux.HandleFunc("/api/history/clear", func(w http.ResponseWriter, r *http.Request) {
		a.tm.clearHistory()
		ok(w)
	})
	mux.HandleFunc("/api/uploads", func(w http.ResponseWriter, r *http.Request) {
		a.uploads.Store(r.URL.Query().Get("on") == "1")
		ok(w)
	})
	mux.HandleFunc("/api/token/regen", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.token = randomToken()
		a.mu.Unlock()
		ok(w)
	})
	mux.HandleFunc("/api/reveal", func(w http.ResponseWriter, r *http.Request) {
		dir := a.getRecvDir()
		os.MkdirAll(dir, 0o755)
		exec.Command("open", dir).Start()
		ok(w)
	})
	mux.HandleFunc("/api/recvdir/pick", func(w http.ResponseWriter, r *http.Request) {
		lines := osascriptLines([]string{"-e", "POSIX path of (choose folder)"})
		if len(lines) > 0 {
			a.mu.Lock()
			a.recvDir = filepath.Clean(lines[0])
			a.mu.Unlock()
		}
		ok(w)
	})

	// CSRF protection: panel is localhost-only, and every state-changing route
	// requires POST plus the custom header the panel sends. GET is not enough,
	// otherwise a page could hit http://127.0.0.1:8081/api/clear via an image.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host != fmt.Sprintf("127.0.0.1:%d", adminPort) && host != fmt.Sprintf("localhost:%d", adminPort) {
			http.Error(w, "forbidden", 403)
			return
		}
		switch r.URL.Path {
		case "/", "/api/state", "/qr.png":
		default:
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
		}
		if r.Method == http.MethodPost && r.Header.Get("X-Req") != "1" {
			http.Error(w, "forbidden", 403)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func main() {
	home, _ := os.UserHomeDir()
	dir := flag.String("dir", filepath.Join(home, "Downloads", "MacShare"), "folder for files received from the phone")
	flag.IntVar(&publicPort, "port", 8080, "port the phone connects to")
	flag.IntVar(&adminPort, "admin-port", 8081, "port of the local control panel")
	noOpen := flag.Bool("no-open", false, "do not open the control panel in the browser")
	flag.Parse()

	app := &App{token: randomToken(), recvDir: *dir, tm: &TransferMgr{}}
	app.uploads.Store(true)
	go app.tm.run()

	// Keep the Mac awake while the app is running.
	exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid())).Start()

	for _, p := range flag.Args() {
		app.add(p)
	}

	ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", publicPort))
	if err != nil {
		log.Fatal(err)
	}
	pub := &http.Server{
		Handler:           app.publicHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, ctxKey{}, c)
		},
	}
	adm := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", adminPort),
		Handler:           app.adminHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() { log.Fatal(pub.Serve(countListener{ln})) }()

	fmt.Println("MacShare is running")
	for _, ip := range localIPs() {
		fmt.Printf("  phone URL (%s): http://%s:%d/%s/\n", ip.Iface, ip.IP, publicPort, app.getToken())
	}
	fmt.Printf("  control panel: http://127.0.0.1:%d\n", adminPort)

	if !*noOpen {
		go func() {
			time.Sleep(300 * time.Millisecond)
			exec.Command("open", fmt.Sprintf("http://127.0.0.1:%d", adminPort)).Start()
		}()
	}
	log.Fatal(adm.ListenAndServe())
}
