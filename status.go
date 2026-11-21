package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

func readAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 64<<20))
}

var pstatus struct {
	sync.Mutex
	attrs map[string][]string
	at    time.Time
	err   string
}

var statusAttrs = []string{"printer-state", "printer-state-reasons", "printer-state-message", "printer-is-accepting-jobs",
	"media-ready", "marker-names", "marker-types", "marker-levels", "printer-make-and-model", "queued-job-count", "printer-up-time"}

func pollPrinter() {
	if a, _ := tgt.get(); a == "" {
		return
	}
	rb, err := printerDo(tgt, buildGPA(tgt.uri(), statusAttrs...))
	var m *msg
	if err == nil {
		m, err = parseIPP(rb)
	}
	pstatus.Lock()
	defer pstatus.Unlock()
	pstatus.at = time.Now()
	if err != nil {
		pstatus.err = err.Error()
		return
	}
	pstatus.err = ""
	pstatus.attrs = map[string][]string{}
	for _, n := range statusAttrs {
		if v := m.strs(n); v != nil {
			pstatus.attrs[n] = v
		}
	}
}

func statusPoller() {
	time.Sleep(5 * time.Second)
	for {
		pollPrinter()
		time.Sleep(20 * time.Second)
	}
}

type statusData struct {
	Version     string              `json:"version"`
	RelayName   string              `json:"relay_name"`
	RelayURL    string              `json:"relay_url"`
	Mode        string              `json:"mode"`
	Uptime      string              `json:"uptime"`
	Printer     string              `json:"printer"`
	PrinterAddr string              `json:"printer_address"`
	FoundAt     string              `json:"discovered_at"`
	MaxQuality  int                 `json:"max_quality"`
	Problem     string              `json:"problem"`
	State       string              `json:"printer_state"`
	Reasons     string              `json:"printer_state_reasons"`
	Paper       string              `json:"paper_loaded"`
	Ribbon      string              `json:"ribbon"`
	PolledAt    string              `json:"polled_at"`
	PollError   string              `json:"poll_error"`
	Attrs       map[string][]string `json:"printer_attributes"`
	Requests    int                 `json:"requests"`
	Jobs        int                 `json:"jobs"`
	Pages       int                 `json:"pages"`
	Errors      int                 `json:"errors"`
	Recent      []jobRec            `json:"recent"`
	Log         []string            `json:"log"`
}

var stateNames = map[string]string{"3": "idle", "4": "printing", "5": "stopped"}

func collect() statusData {
	d := statusData{Version: version, RelayName: relayName(), Mode: *mode,
		Uptime:   time.Since(started).Round(time.Second).String(),
		RelayURL: fmt.Sprintf("ipp://%s.local:%d", hostname, listenPt)}
	tgt.mu.RLock()
	d.Printer, d.PrinterAddr, d.MaxQuality, d.Problem = tgt.instance, tgt.addr+tgt.path, tgt.maxQ, tgt.lastError
	if !tgt.found.IsZero() {
		d.FoundAt = tgt.found.Format("Jan 2 15:04:05")
	}
	tgt.mu.RUnlock()
	pstatus.Lock()
	d.Attrs, d.PollError = pstatus.attrs, pstatus.err
	if !pstatus.at.IsZero() {
		d.PolledAt = pstatus.at.Format("15:04:05")
	}
	pstatus.Unlock()
	if d.Attrs != nil {
		s := strings.Join(d.Attrs["printer-state"], "")
		if n, ok := stateNames[s]; ok {
			s = n
		}
		d.State = s
		d.Reasons = strings.Join(d.Attrs["printer-state-reasons"], ", ")
		d.Paper = strings.Join(d.Attrs["media-ready"], ", ")
		var rib []string
		for i, n := range d.Attrs["marker-names"] {
			lv := ""
			if i < len(d.Attrs["marker-levels"]) {
				switch l := d.Attrs["marker-levels"][i]; l {
				case "-1":
					lv = "level not reported"
				case "-2":
					lv = "level unknown"
				case "-3":
					lv = "OK (exact level not reported)"
				default:
					lv = l + "%"
				}
			}
			rib = append(rib, n+": "+lv)
		}
		d.Ribbon = strings.Join(rib, "; ")
	}
	counters.Lock()
	d.Requests, d.Jobs, d.Pages, d.Errors = counters.requests, counters.jobs, counters.pages, counters.errors
	counters.Unlock()
	d.Recent = jobs.get()
	lg := logs.get()
	if len(lg) > 60 {
		lg = lg[len(lg)-60:]
	}
	d.Log = lg
	return d
}

func statusJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	e.Encode(collect())
}

var page = template.Must(template.New("p").Funcs(template.FuncMap{
	"ok": func(s string) bool { return s == "none" || s == "" },
	"t":  func(t time.Time) string { return t.Format("15:04:05") },
	"good": func(s string) bool {
		return strings.HasPrefix(s, "0x00")
	},
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="15"><title>{{.RelayName}}</title><style>
:root{--bg:#f6f7f9;--card:#fff;--fg:#1d2230;--mut:#6b7280;--line:#e5e7eb;--ok:#15803d;--bad:#b91c1c;--warn:#b45309}
@media(prefers-color-scheme:dark){:root{--bg:#111318;--card:#1a1d24;--fg:#e6e8ee;--mut:#9aa1ad;--line:#2a2f3a;--ok:#4ade80;--bad:#f87171;--warn:#fbbf24}}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 -apple-system,system-ui,Segoe UI,sans-serif}
main{max-width:860px;margin:0 auto;padding:20px}h1{font-size:20px;margin:0 0 4px}h2{font-size:13px;text-transform:uppercase;letter-spacing:.06em;color:var(--mut);margin:0 0 10px}
.sub{color:var(--mut);margin-bottom:18px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:14px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px}
dl{display:grid;grid-template-columns:auto 1fr;gap:4px 14px;margin:0}dt{color:var(--mut)}dd{margin:0;word-break:break-word}
.ok{color:var(--ok)}.bad{color:var(--bad)}.warn{color:var(--warn)}table{width:100%;border-collapse:collapse}
td,th{text-align:left;padding:5px 6px;border-bottom:1px solid var(--line);vertical-align:top}th{color:var(--mut);font-weight:500}
pre{margin:0;max-height:320px;overflow:auto;font:12px/1.4 ui-monospace,Menlo,monospace;white-space:pre-wrap}
.nums{display:flex;gap:22px}.nums b{font-size:20px;display:block}</style></head><body><main>
<h1>{{.RelayName}}</h1><div class="sub">selphy-relay {{.Version}} · up {{.Uptime}} · refreshes every 15 s · <a href="/status.json">JSON</a></div>
{{if .Problem}}<div class="card warn">{{.Problem}}</div>{{end}}
<div class="grid"><div class="card"><h2>Printer</h2><dl>
<dt>Name</dt><dd>{{or .Printer "—"}}</dd><dt>Address</dt><dd>{{or .PrinterAddr "—"}}</dd>
<dt>State</dt><dd>{{if .PollError}}<span class="bad">unreachable: {{.PollError}}</span>{{else}}{{or .State "—"}}{{end}}</dd>
<dt>Problems</dt><dd>{{if ok .Reasons}}<span class="ok">none</span>{{else}}<span class="bad">{{.Reasons}}</span>{{end}}</dd>
<dt>Paper</dt><dd>{{or .Paper "—"}}</dd><dt>Ribbon</dt><dd>{{or .Ribbon "—"}}</dd>
<dt>Checked</dt><dd>{{or .PolledAt "—"}}</dd></dl></div>
<div class="card"><h2>Relay</h2><dl><dt>Mode</dt><dd>{{.Mode}}</dd><dt>Quality cap</dt><dd>{{.MaxQuality}}</dd>
<dt>Discovered</dt><dd>{{or .FoundAt "—"}}</dd><dt>Address</dt><dd>{{.RelayURL}}</dd></dl>
<div class="nums" style="margin-top:12px"><div><b>{{.Jobs}}</b>jobs</div><div><b>{{.Pages}}</b>pages</div><div><b>{{.Errors}}</b>errors</div><div><b>{{.Requests}}</b>requests</div></div></div></div>
<div class="card"><h2>Recent activity</h2>{{if .Recent}}<table><tr><th>Time</th><th>Operation</th><th>Result</th><th>Details</th><th>From</th></tr>
{{range .Recent}}<tr><td>{{t .Time}}</td><td>{{.Op}}</td><td class="{{if good .Status}}ok{{else}}bad{{end}}">{{.Status}}</td><td>{{.Info}}</td><td>{{.Client}}</td></tr>{{end}}</table>
{{else}}<div class="sub">No jobs yet.</div>{{end}}</div>
<div class="card"><h2>Log</h2><pre>{{range .Log}}{{.}}
{{end}}</pre></div></main></body></html>`))

func statusHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page.Execute(w, collect())
}
