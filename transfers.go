package main

import (
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// countConn wraps a TCP connection and counts bytes moved through it.
// ReadFrom is forwarded to the underlying TCPConn so sendfile(2) keeps working.
type countConn struct {
	*net.TCPConn
	written atomic.Int64
	read    atomic.Int64
}

func (c *countConn) Write(p []byte) (int, error) {
	n, err := c.TCPConn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.TCPConn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// ReadFrom forwards to TCPConn.ReadFrom (sendfile) but in 4 MB chunks, so the
// byte counter advances while a large file is in flight instead of only at the end.
func (c *countConn) ReadFrom(r io.Reader) (int64, error) {
	const chunk = 4 << 20
	src, remain := r, int64(-1)
	lr, limited := r.(*io.LimitedReader)
	if limited {
		src, remain = lr.R, lr.N
	}
	var total int64
	for remain != 0 {
		want := int64(chunk)
		if remain > 0 && remain < want {
			want = remain
		}
		n, err := c.TCPConn.ReadFrom(io.LimitReader(src, want))
		c.written.Add(n)
		total += n
		if remain > 0 {
			remain -= n
		}
		if err != nil {
			if limited {
				lr.N -= total
			}
			return total, err
		}
		if n < want { // source exhausted
			break
		}
	}
	if limited {
		lr.N -= total
	}
	return total, nil
}

type countListener struct{ net.Listener }

func (l countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := c.(*net.TCPConn)
	tc.SetNoDelay(true)
	tc.SetWriteBuffer(sockBuf)
	tc.SetReadBuffer(sockBuf)
	return &countConn{TCPConn: tc}, nil
}

// Transfer describes one file transfer or speed-test stream.
type Transfer struct {
	ID      int     `json:"id"`
	Kind    string  `json:"kind"` // "file" or "test"
	Dir     string  `json:"dir"`  // "send" (Mac to phone) or "receive"
	Name    string  `json:"name"`
	Peer    string  `json:"peer"`
	Total   int64   `json:"total"` // -1 when unknown
	Done    int64   `json:"done"`
	Speed   float64 `json:"speed"` // smoothed bytes/s
	Peak    float64 `json:"peak"`
	Avg     float64 `json:"avg"`
	ETA     float64 `json:"eta"` // seconds, -1 when unknown
	State   string  `json:"state"`
	Started int64   `json:"started"` // unix ms
	Ended   int64   `json:"ended"`
	src     func() int64
	conn    *countConn
	last    int64
	sampled bool
}

const graphLen = 120 // samples kept (500 ms each, so 60 s)

type TransferMgr struct {
	mu        sync.Mutex
	nextID    int
	active    []*Transfer
	history   []Transfer
	graphSend []float64
	graphRecv []float64
	sendNow   float64
	recvNow   float64
	peakSend  float64
	peakRecv  float64
	totalSent int64
	totalRecv int64
}

type Snapshot struct {
	Active    []Transfer `json:"active"`
	History   []Transfer `json:"history"`
	GraphSend []float64  `json:"graphSend"`
	GraphRecv []float64  `json:"graphRecv"`
	SendNow   float64    `json:"sendNow"`
	RecvNow   float64    `json:"recvNow"`
	PeakSend  float64    `json:"peakSend"`
	PeakRecv  float64    `json:"peakRecv"`
	TotalSent int64      `json:"totalSent"`
	TotalRecv int64      `json:"totalRecv"`
}

func (m *TransferMgr) start(kind, dir, name, peer string, total int64, src func() int64, conn *countConn) *Transfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	t := &Transfer{
		ID: m.nextID, Kind: kind, Dir: dir, Name: name, Peer: peer, Total: total,
		State: "active", Started: time.Now().UnixMilli(), ETA: -1, src: src, conn: conn,
	}
	m.active = append(m.active, t)
	return t
}

func (m *TransferMgr) finish(t *Transfer, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := t.src()
	if t.Total > 0 && cur > t.Total {
		cur = t.Total
	}
	t.Done = cur
	t.Ended = time.Now().UnixMilli()
	dur := float64(t.Ended-t.Started) / 1000
	if dur < 0.001 {
		dur = 0.001
	}
	t.Avg = float64(cur) / dur
	t.Speed, t.ETA = 0, -1
	if ok {
		t.State = "done"
	} else {
		t.State = "aborted"
	}
	for i, a := range m.active {
		if a == t {
			m.active = append(m.active[:i], m.active[i+1:]...)
			break
		}
	}
	if t.Dir == "send" {
		m.totalSent += cur
	} else {
		m.totalRecv += cur
	}
	if t.Kind == "file" && cur > 0 {
		m.history = append([]Transfer{*t}, m.history...)
		if len(m.history) > 200 {
			m.history = m.history[:200]
		}
	}
}

func (m *TransferMgr) cancel(id int) {
	m.mu.Lock()
	var c *countConn
	for _, t := range m.active {
		if t.ID == id {
			c = t.conn
		}
	}
	m.mu.Unlock()
	if c != nil && c.TCPConn != nil {
		c.Close()
	}
}

func (m *TransferMgr) clearHistory() {
	m.mu.Lock()
	m.history = nil
	m.mu.Unlock()
}

func pushCap(s []float64, v float64) []float64 {
	s = append(s, v)
	if len(s) > graphLen {
		s = s[len(s)-graphLen:]
	}
	return s
}

func (m *TransferMgr) sample(dt float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s, r float64
	for _, t := range m.active {
		cur := t.src()
		inst := float64(cur-t.last) / dt
		if !t.sampled {
			t.Speed, t.sampled = inst, true
		} else {
			t.Speed = 0.5*t.Speed + 0.5*inst
		}
		t.last, t.Done = cur, cur
		if t.Speed > t.Peak {
			t.Peak = t.Speed
		}
		if t.Total > cur && t.Speed > 1 {
			t.ETA = float64(t.Total-cur) / t.Speed
		} else {
			t.ETA = -1
		}
		if t.Dir == "send" {
			s += t.Speed
		} else {
			r += t.Speed
		}
	}
	m.sendNow, m.recvNow = s, r
	m.graphSend = pushCap(m.graphSend, s)
	m.graphRecv = pushCap(m.graphRecv, r)
	if s > m.peakSend {
		m.peakSend = s
	}
	if r > m.peakRecv {
		m.peakRecv = r
	}
}

func (m *TransferMgr) run() {
	tick := time.NewTicker(500 * time.Millisecond)
	last := time.Now()
	for now := range tick.C {
		dt := now.Sub(last).Seconds()
		last = now
		if dt > 0 {
			m.sample(dt)
		}
	}
}

func (m *TransferMgr) snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{
		Active:    make([]Transfer, 0, len(m.active)),
		History:   append([]Transfer{}, m.history...),
		GraphSend: append([]float64{}, m.graphSend...),
		GraphRecv: append([]float64{}, m.graphRecv...),
		SendNow:   m.sendNow, RecvNow: m.recvNow,
		PeakSend: m.peakSend, PeakRecv: m.peakRecv,
		TotalSent: m.totalSent, TotalRecv: m.totalRecv,
	}
	for _, t := range m.active {
		s.Active = append(s.Active, *t)
		if t.Dir == "send" {
			s.TotalSent += t.Done
		} else {
			s.TotalRecv += t.Done
		}
	}
	sort.Slice(s.Active, func(i, j int) bool { return s.Active[i].Started < s.Active[j].Started })
	return s
}
