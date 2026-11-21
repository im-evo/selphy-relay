// selphy-relay: AirPrint relay for printers with broken IPP servers, built for
// the Canon SELPHY CP1200. See README.md.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "1.0.0"

var (
	listen   = flag.String("listen", ":8631", "address to listen on")
	printerF = flag.String("printer", "", "printer host:port (empty = discover via mDNS)")
	pathF    = flag.String("path", "", "printer resource path (empty = from discovery, else /ipp/print)")
	match    = flag.String("match", "SELPHY", "discover the first printer whose name or model contains this text")
	iface    = flag.String("iface", "br-lan", "LAN interface for discovery")
	mode     = flag.String("mode", "jpeg", "URF handling: jpeg (convert), patch (force quality only), off (pass through)")
	jpegQ    = flag.Int("jpeg-quality", 95, "JPEG quality for URF conversion")
	qualityF = flag.Int("quality", 0, "maximum print-quality to send (0 = what the printer supports)")
	nameF    = flag.String("name", "", "relay printer name (empty = \"<printer name> Relay\")")
	avahiDir = flag.String("avahi-dir", "/etc/avahi/services", "write an avahi service file here (empty = don't)")
	rediscov = flag.Duration("rediscover", 2*time.Minute, "how often to re-check the printer's address")
	advJPEG  = flag.Bool("advertise-jpeg", false, "hide image/urf from clients (iOS 26 refuses this; kept for other clients)")
)

var (
	tgt      = &target{}
	logs     = &logRing{max: 200}
	jobs     = &jobRing{}
	started  = time.Now()
	hostname string
	listenPt int
	counters struct {
		sync.Mutex
		requests, jobs, pages, errors int
	}
	createJobs = struct {
		sync.Mutex
		m map[int]storedJob
	}{m: map[int]storedJob{}}
	pageQueue = make(chan pageJob, 64)
)

type storedJob struct {
	req *msg
	at  time.Time
}

type pageJob struct {
	m      *msg
	client string
	label  string
}

func relayName() string {
	if *nameF != "" {
		return *nameF
	}
	tgt.mu.RLock()
	defer tgt.mu.RUnlock()
	if tgt.instance != "" {
		return tgt.instance + " Relay"
	}
	return "Printer Relay"
}

func relayUUID() string {
	tgt.mu.RLock()
	defer tgt.mu.RUnlock()
	return derivedUUID(tgt.uuid + "|" + tgt.instance)
}

func bump(f func()) { counters.Lock(); f(); counters.Unlock() }

// ---------------- discovery ----------------

func discoverOnce() {
	svcs, err := discoverIPP(*iface)
	if err != nil {
		setErr("discovery: " + err.Error())
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].instance < svcs[j].instance })
	var pick *service
	for i := range svcs {
		s := &svcs[i]
		if s.txtGet("relay") != "" || s.ip == nil {
			continue // skip ourselves / other relays and unresolved hosts
		}
		if *printerF != "" {
			h, _, _ := net.SplitHostPort(*printerF)
			if s.ip.String() == h {
				pick = s
				break
			}
			continue
		}
		m := strings.ToLower(*match)
		if strings.Contains(strings.ToLower(s.instance), m) || strings.Contains(strings.ToLower(s.txtGet("ty")), m) {
			pick = s
			break
		}
	}
	tgt.mu.Lock()
	prevAddr := tgt.addr
	switch {
	case pick != nil:
		addr := net.JoinHostPort(pick.ip.String(), strconv.Itoa(pick.port))
		if *printerF != "" {
			addr = *printerF
		}
		path := "/" + strings.TrimPrefix(pick.txtGet("rp"), "/")
		if pick.txtGet("rp") == "" {
			path = "/ipp/print"
		}
		if *pathF != "" {
			path = *pathF
		}
		tgt.addr, tgt.path, tgt.instance, tgt.txt = addr, path, pick.instance, pick.txt
		tgt.uuid = pick.txtGet("UUID")
		tgt.found, tgt.lastError = time.Now(), ""
	case *printerF != "" && tgt.addr == "":
		tgt.addr, tgt.path = *printerF, "/ipp/print"
		if *pathF != "" {
			tgt.path = *pathF
		}
		tgt.lastError = "printer not seen via mDNS; using configured address"
	case tgt.addr == "":
		tgt.lastError = fmt.Sprintf("no printer matching %q found on %s", *match, *iface)
	}
	addr := tgt.addr
	tgt.mu.Unlock()
	if addr != "" && addr != prevAddr {
		log.Printf("printer: %s at %s", tgt.instance, addr)
		refreshPrinterInfo()
	}
	if addr != "" {
		writeAvahi()
	}
}

func setErr(s string) {
	tgt.mu.Lock()
	tgt.lastError = s
	tgt.mu.Unlock()
	log.Print(s)
}

// refreshPrinterInfo reads what discovery can't give: supported quality, and a
// TXT record fallback when the printer was configured by address only.
func refreshPrinterInfo() {
	rb, err := printerDo(tgt, buildGPA(tgt.uri(), "print-quality-supported", "printer-uuid", "printer-info",
		"printer-make-and-model", "document-format-supported", "urf-supported", "color-supported"))
	if err != nil {
		setErr("printer query: " + err.Error())
		return
	}
	m, err := parseIPP(rb)
	if err != nil {
		setErr("printer query: " + err.Error())
		return
	}
	maxQ := 0
	for _, v := range m.strs("print-quality-supported") {
		if q, _ := strconv.Atoi(v); q > maxQ {
			maxQ = q
		}
	}
	tgt.mu.Lock()
	tgt.maxQ = maxQ
	if len(tgt.txt) == 0 { // configured by address, not discovered
		tgt.instance = m.str("printer-info")
		tgt.uuid = strings.TrimPrefix(m.str("printer-uuid"), "urn:uuid:")
		col := "F"
		if m.str("color-supported") == "\x01" {
			col = "T"
		}
		tgt.txt = []string{"txtvers=1", "qtotal=1", "rp=" + strings.TrimPrefix(tgt.path, "/"),
			"ty=" + m.str("printer-make-and-model"), "product=(" + m.str("printer-make-and-model") + ")",
			"pdl=" + strings.Join(m.strs("document-format-supported"), ","),
			"URF=" + strings.Join(m.strs("urf-supported"), ","), "Color=" + col, "UUID=" + tgt.uuid}
	}
	tgt.mu.Unlock()
	log.Printf("printer supports print-quality up to %d", maxQ)
}

func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func writeAvahi() {
	if *avahiDir == "" {
		return
	}
	tgt.mu.RLock()
	txt := append([]string(nil), tgt.txt...)
	tgt.mu.RUnlock()
	var recs []string
	for _, t := range txt {
		k, _, _ := strings.Cut(t, "=")
		switch strings.ToLower(k) {
		case "uuid", "adminurl", "tls", "relay", "note":
			continue
		}
		recs = append(recs, t)
	}
	recs = append(recs, "UUID="+relayUUID(), fmt.Sprintf("adminurl=http://%s.local:%d/", hostname, listenPt),
		"note=via "+hostname, "relay=selphy-relay")
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" standalone='no'?>\n<!DOCTYPE service-group SYSTEM \"avahi-service.dtd\">\n<service-group>\n")
	fmt.Fprintf(&b, "  <name>%s</name>\n  <service>\n    <type>_ipp._tcp</type>\n    <subtype>_universal._sub._ipp._tcp</subtype>\n    <port>%d</port>\n",
		xmlEsc(relayName()), listenPt)
	for _, r := range recs {
		fmt.Fprintf(&b, "    <txt-record>%s</txt-record>\n", xmlEsc(r))
	}
	b.WriteString("  </service>\n</service-group>\n")
	file := filepath.Join(*avahiDir, "selphy-relay.service")
	if old, err := os.ReadFile(file); err == nil && string(old) == b.String() {
		return
	}
	os.MkdirAll(*avahiDir, 0755)
	if err := os.WriteFile(file, []byte(b.String()), 0644); err != nil {
		setErr("avahi: " + err.Error())
		return
	}
	log.Printf("avahi: published %q on port %d", relayName(), listenPt)
	reloadAvahi()
}

func reloadAvahi() {
	for _, pf := range []string{"/var/run/avahi-daemon/pid", "/run/avahi-daemon/pid"} {
		if b, err := os.ReadFile(pf); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				if syscall.Kill(pid, syscall.SIGHUP) == nil {
					return
				}
			}
		}
	}
	exec.Command("killall", "-HUP", "avahi-daemon").Run()
}

// ---------------- extra pages ----------------

func pageWorker() {
	for pj := range pageQueue {
		deadline := time.Now().Add(15 * time.Minute)
		for {
			pj.m.setOpAttr(0x45, "printer-uri", []byte(tgt.uri()))
			rb, err := printerDo(tgt, pj.m.encode())
			st := "error"
			if err == nil {
				if rm, perr := parseIPP(rb); perr == nil {
					st = fmt.Sprintf("0x%04x", rm.status())
					if rm.status() == 0x0507 || rm.status() == 0x0506 { // busy / not accepting
						if time.Now().Before(deadline) {
							time.Sleep(5 * time.Second)
							continue
						}
					}
				}
			} else {
				st = err.Error()
			}
			log.Printf("Print-Job %s: status %s", pj.label, st)
			jobs.add(jobRec{time.Now(), pj.client, "Print-Job", st, pj.label})
			bump(func() { counters.pages++ })
			break
		}
	}
}

func cloneMsg(m *msg) *msg {
	c := &msg{head: m.head}
	for _, g := range m.gs {
		c.gs = append(c.gs, append([]attr(nil), g...))
	}
	return c
}

// ---------------- the relay ----------------

func handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/", "/index.html":
			statusHTML(w)
		case "/status.json":
			statusJSON(w)
		default:
			http.NotFound(w, r)
		}
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bump(func() { counters.requests++ })
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := parseIPP(body)
	if err != nil {
		log.Printf("bad request from %s: %v", r.RemoteAddr, err)
		http.Error(w, "bad ipp", http.StatusBadRequest)
		return
	}
	op := req.op()
	_, ppath := tgt.get()
	host := r.Host
	if host == "" {
		host = fmt.Sprintf("%s.local:%d", hostname, listenPt)
	}
	relayURI := "ipp://" + host + ppath
	printerURI := tgt.uri()

	// --- rewrite the request ---
	req.setOpAttr(0x45, "printer-uri", []byte(printerURI))
	wantDB := false
	if i := req.find("requested-attributes"); i >= 0 {
		var keep []attr
		hasReady := false
		for _, a := range req.gs[i] {
			switch string(a.value) {
			case "media-col-database":
				wantDB = true
				continue
			case "all", "media-col-ready":
				hasReady = true
			}
			keep = append(keep, a)
		}
		if wantDB && !hasReady {
			keep = append(keep, attr{0x44, "", []byte("media-col-ready")})
		}
		keep[0].name = "requested-attributes"
		for k := 1; k < len(keep); k++ {
			keep[k].name = ""
		}
		req.gs[i] = keep
	}
	maxQ := *qualityF
	if maxQ == 0 {
		tgt.mu.RLock()
		maxQ = tgt.maxQ
		tgt.mu.RUnlock()
	}
	if *mode != "off" && maxQ > 0 {
		if i := req.find("print-quality"); i >= 0 && len(req.gs[i][0].value) == 4 {
			if v := int(binary.BigEndian.Uint32(req.gs[i][0].value)); v > maxQ {
				binary.BigEndian.PutUint32(req.gs[i][0].value, uint32(maxQ))
			}
		}
	}
	info := ""
	var extra [][]byte
	isURF := req.str("document-format") == "image/urf"
	if isURF && *mode == "jpeg" {
		if op == 2 || op == 6 {
			if len(req.data) > 0 {
				pages, err := decodeURF(req.data)
				if err != nil && len(pages) == 0 {
					log.Printf("%s: URF decode failed, forwarding as-is: %v", opName(op), err)
				} else {
					if err != nil {
						log.Printf("%s: URF partially decoded (%d pages): %v", opName(op), len(pages), err)
					}
					var jp [][]byte
					for _, p := range pages {
						j, err := encodeJPEG(p, *jpegQ)
						if err != nil {
							log.Printf("%s: JPEG encode failed: %v", opName(op), err)
							break
						}
						jp = append(jp, j)
					}
					if len(jp) > 0 {
						info = fmt.Sprintf("URF %d page(s) %s -> JPEG", len(pages), pages[0])
						log.Printf("%s: converted %s, %d KB", opName(op), info, len(jp[0])/1024)
						req.data = jp[0]
						req.setOpAttr(0x49, "document-format", []byte("image/jpeg"))
						extra = jp[1:]
					}
				}
			}
		} else {
			req.setOpAttr(0x49, "document-format", []byte("image/jpeg"))
		}
	}
	if isURF && *mode == "patch" && (op == 2 || op == 6) && len(req.data) >= 44 && maxQ > 0 && int(req.data[15]) > maxQ {
		req.data[15] = byte(maxQ)
	}

	// --- forward ---
	rb, err := printerDo(tgt, req.encode())
	if err != nil {
		bump(func() { counters.errors++ })
		log.Printf("%s: printer error: %v", opName(op), err)
		http.Error(w, "printer unreachable", http.StatusBadGateway)
		return
	}
	rm, err := parseIPP(rb)
	if err != nil {
		bump(func() { counters.errors++ })
		log.Printf("%s: unparseable printer reply (%d bytes): %v; passing through", opName(op), len(rb), err)
		w.Header().Set("Content-Type", "application/ipp")
		w.Write(rb)
		return
	}

	// --- rewrite the reply ---
	for gi, g := range rm.gs {
		for ai, a := range g {
			if a.tag == 0x45 && strings.HasPrefix(string(a.value), printerURI) {
				rm.gs[gi][ai].value = []byte(relayURI + strings.TrimPrefix(string(a.value), printerURI))
			}
		}
	}
	if i := rm.find("printer-uuid"); i >= 0 {
		rm.gs[i][0].value = []byte("urn:uuid:" + relayUUID())
	}
	if i := rm.find("printer-dns-sd-name"); i >= 0 {
		rm.gs[i][0].value = []byte(relayName())
	}
	if *advJPEG {
		if i := rm.find("document-format-supported"); i >= 0 {
			var keep []attr
			for _, a := range rm.gs[i] {
				if string(a.value) != "image/urf" {
					keep = append(keep, a)
				}
			}
			if len(keep) > 0 {
				keep[0].name = "document-format-supported"
				for k := 1; k < len(keep); k++ {
					keep[k].name = ""
				}
				rm.gs[i] = keep
			}
		}
		for _, n := range []string{"document-format-default", "document-format-preferred"} {
			if i := rm.find(n); i >= 0 {
				rm.gs[i][0].value = []byte("image/jpeg")
			}
		}
	}
	if wantDB && rm.find("media-col-database") < 0 {
		if i := rm.find("media-col-ready"); i >= 0 {
			cp := append([]attr(nil), rm.gs[i]...)
			cp[0].name = "media-col-database"
			rm.gs = append(rm.gs[:i+1], append([][]attr{cp}, rm.gs[i+1:]...)...)
		}
	}
	st := rm.status()
	if op == 5 && st < 0x0100 { // Create-Job: remember attributes for extra pages
		if id, ok := rm.int("job-id"); ok {
			createJobs.Lock()
			for k, v := range createJobs.m {
				if time.Since(v.at) > time.Hour {
					delete(createJobs.m, k)
				}
			}
			createJobs.m[id] = storedJob{cloneMsg(req), time.Now()}
			createJobs.Unlock()
		}
	}
	if (op == 2 || op == 6) && st < 0x0100 {
		bump(func() { counters.jobs++; counters.pages++ })
	}
	if len(extra) > 0 && st < 0x0100 {
		var base *msg
		if op == 6 {
			if id, ok := req.int("job-id"); ok {
				createJobs.Lock()
				if s, ok := createJobs.m[id]; ok {
					base = cloneMsg(s.req)
				}
				createJobs.Unlock()
			}
			if base == nil {
				base = cloneMsg(req)
				base.remove("job-id")
				base.remove("last-document")
			}
		} else {
			base = cloneMsg(req)
		}
		name := base.str("job-name")
		for k, j := range extra {
			m := cloneMsg(base)
			m.setOp(2)
			m.setOpAttr(0x49, "document-format", []byte("image/jpeg"))
			m.setOpAttr(0x42, "job-name", []byte(fmt.Sprintf("%s (page %d)", name, k+2)))
			m.data = j
			pageQueue <- pageJob{m, r.RemoteAddr, fmt.Sprintf("page %d of %d", k+2, len(extra)+1)}
		}
		log.Printf("%s: queued %d more page(s) as separate jobs", opName(op), len(extra))
	}
	if op != 11 && op != 9 { // keep the job list readable: skip status polls
		jobs.add(jobRec{time.Now(), r.RemoteAddr, opName(op), fmt.Sprintf("0x%04x", st), info})
	}
	if op != 11 && op != 9 || st >= 0x0400 {
		log.Printf("%s from %s: status 0x%04x %s", opName(op), r.RemoteAddr, st, info)
	}
	w.Header().Set("Content-Type", "application/ipp")
	w.Write(rm.encode())
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	log.SetOutput(logs)
	hostname, _ = os.Hostname()
	if _, p, err := net.SplitHostPort(*listen); err == nil {
		listenPt, _ = strconv.Atoi(p)
	}
	switch *mode {
	case "jpeg", "patch", "off":
	default:
		log.Fatalf("unknown -mode %q", *mode)
	}
	log.Printf("selphy-relay %s listening on %s, mode %s", version, *listen, *mode)
	go pageWorker()
	go statusPoller()
	go func() {
		for {
			discoverOnce()
			time.Sleep(*rediscov)
		}
	}()
	srv := &http.Server{Addr: *listen, Handler: http.HandlerFunc(handle), ReadHeaderTimeout: 30 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
