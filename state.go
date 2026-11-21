package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---- ring buffers for the status page ----

type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (l *logRing) Write(p []byte) (int, error) {
	os.Stderr.Write(p)
	l.mu.Lock()
	for _, s := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		l.lines = append(l.lines, time.Now().Format("15:04:05 ")+s)
	}
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	l.mu.Unlock()
	return len(p), nil
}

func (l *logRing) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

type jobRec struct {
	Time   time.Time `json:"time"`
	Client string    `json:"client"`
	Op     string    `json:"op"`
	Status string    `json:"status"`
	Info   string    `json:"info"`
}

type jobRing struct {
	mu   sync.Mutex
	recs []jobRec
}

func (j *jobRing) add(r jobRec) {
	j.mu.Lock()
	j.recs = append(j.recs, r)
	if len(j.recs) > 30 {
		j.recs = j.recs[len(j.recs)-30:]
	}
	j.mu.Unlock()
}

func (j *jobRing) get() []jobRec {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]jobRec, len(j.recs))
	for i := range j.recs {
		out[i] = j.recs[len(j.recs)-1-i]
	}
	return out
}

// ---- printer target, discovered or configured ----

type target struct {
	mu        sync.RWMutex
	addr      string // host:port
	path      string
	instance  string
	txt       []string
	uuid      string // printer's UUID (no urn: prefix)
	maxQ      int
	found     time.Time
	lastError string
}

func (t *target) get() (addr, path string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.addr, t.path
}

func (t *target) uri() string {
	a, p := t.get()
	return "ipp://" + a + p
}

// ---- serialized access to the printer ----

// The CP1200's IPP server corrupts replies when requests overlap, so every
// request goes through one lock.
var printerMu sync.Mutex

var httpc = &http.Client{
	Timeout:   10 * time.Minute,
	Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true},
}

func printerDo(t *target, body []byte) ([]byte, error) {
	addr, path := t.get()
	if addr == "" {
		return nil, fmt.Errorf("printer not found yet")
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/ipp")
	printerMu.Lock()
	defer printerMu.Unlock()
	start := time.Now()
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if d := time.Since(start); d > 5*time.Second {
		log.Printf("printer took %v", d.Round(time.Millisecond))
	}
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("printer HTTP %s", resp.Status)
	}
	return rb, err
}

// derivedUUID gives the relay a stable UUID of its own, based on the printer's.
func derivedUUID(printerUUID string) string {
	h := sha1.Sum([]byte("selphy-relay:" + strings.ToLower(printerUUID)))
	h[6] = (h[6] & 0x0f) | 0x50
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
