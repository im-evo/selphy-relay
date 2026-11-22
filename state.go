package main

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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
	Timeout: 10 * time.Minute, // a print job's reply can take a while
	Transport: &http.Transport{
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext:        (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	},
}

// ---- printer reachability ----

var errOffline = errors.New("printer is offline")

var reach struct {
	sync.Mutex
	known   bool // at least one attempt made
	online  bool
	since   time.Time // when the current state began
	tried   time.Time // last real connection attempt
	lastErr string
}

// note records the outcome of a real connection attempt and logs state changes only.
func note(err error) {
	reach.Lock()
	defer reach.Unlock()
	reach.tried = time.Now()
	online := err == nil
	if !reach.known || online != reach.online {
		if online {
			if reach.known {
				log.Printf("printer back online")
				go discoverOnce() // pick up details missed while it was off
			}
		} else {
			log.Printf("printer offline: %v", err)
		}
		reach.known, reach.online, reach.since = true, online, time.Now()
	}
	if err != nil {
		reach.lastErr = err.Error()
	}
}

// skipOffline reports whether to fail fast: the printer was offline at the last
// attempt, made less than 10 seconds ago.
func skipOffline() bool {
	reach.Lock()
	defer reach.Unlock()
	return reach.known && !reach.online && time.Since(reach.tried) < 10*time.Second
}

func printerOnline() (online, known bool, since time.Time) {
	reach.Lock()
	defer reach.Unlock()
	return reach.online, reach.known, reach.since
}

// printerDo sends one IPP request to the printer. probe=true always attempts a
// connection (status poller); otherwise requests fail fast while the printer is offline.
func printerDo(t *target, body []byte, probe bool) ([]byte, error) {
	addr, path := t.get()
	if addr == "" {
		return nil, errOffline
	}
	if !probe && skipOffline() {
		return nil, errOffline
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/ipp")
	printerMu.Lock()
	defer printerMu.Unlock()
	start := time.Now()
	resp, err := httpc.Do(req)
	if err != nil { // couldn't connect or the connection broke: treat as offline
		note(err)
		return nil, errOffline
	}
	note(nil)
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
