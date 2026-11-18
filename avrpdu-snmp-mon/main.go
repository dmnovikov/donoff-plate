// donoff-pdu-view: a page for an ATS/PDU by SNMP (the model by a profile: RCNTEC RPCM built in).
// It polls the RPCM by SNMP GET (never SET), shows the inputs and the outlets as tiles, and keeps
// a small history in InfluxDB 2 for the charts (no InfluxDB: tiles only).
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

var version = "0.1.0"

//go:embed web
var webFS embed.FS

func main() {
	var (
		host      = flag.String("host", "", "the device address, host or host:port (SNMP port 161 by default)")
		hostOld   = flag.String("rpcm", "", "the same as -host (the old name)")
		community = flag.String("community", "public", "SNMP v2c community to read with")
		listen    = flag.String("listen", ":8095", "address of the page")
		name      = flag.String("name", "pdu1", "the device name in InfluxDB (tag dev)")
		mainIn    = flag.Int("main", 1, "the main input of the ATS (1 or 2)")
		iURL      = flag.String("influx-url", "http://127.0.0.1:8086", "InfluxDB 2 URL")
		iOrg      = flag.String("influx-org", "nnz", "InfluxDB organization")
		iBucket   = flag.String("influx-bucket", "pdu", "InfluxDB bucket")
		iToken    = flag.String("influx-token-file", "/etc/donoff-pdu-view/influx.token", "file with the InfluxDB token (no file: no history)")
		profID    = flag.String("profile", "auto", "the model profile id, or auto (by sysObjectID)")
		profDir   = flag.String("profiles", "", "a folder with more profiles (*.yaml); the same id replaces a built-in one")
		listProf  = flag.Bool("list-profiles", false, "print the profiles and exit")
		control   = flag.String("control", "", "outlets this page may switch, e.g. 1 or 1,3 (empty: read only)")
		minGap    = flag.Duration("min-gap", 10*time.Second, "the least time between two switches of an outlet")
		maxHour   = flag.Int("max-hour", 20, "the most switches of an outlet in an hour")
		showVer   = flag.Bool("version", false, "print the version")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("donoff-pdu-view", version)
		return
	}
	if *host == "" {
		*host = *hostOld
	}
	debug.SetMemoryLimit(24 << 20) // a small board: collect early

	all, err := loadProfiles(*profDir)
	if err != nil {
		log.Fatal(err)
	}
	if *listProf {
		for _, id := range strings.Split(profileIDs(all), ", ") {
			pr := all[id]
			fmt.Printf("%-16s %s %s: %s (sysObjectID %s)\n", pr.ID, pr.Vendor, pr.Model, pr.Title, strings.Join(pr.Match, ", "))
		}
		return
	}
	if *host == "" {
		log.Fatal("give the device address: -host 192.168.0.78")
	}
	var prof *Profile
	if *profID != "auto" {
		if prof = all[*profID]; prof == nil {
			log.Fatalf("no profile %q (there are: %s)", *profID, profileIDs(all))
		}
	}
	p := newPoller(*host, *community, *mainIn, prof, all)
	g, err := newGuard(*control, *minGap, max(*maxHour, 1))
	if err != nil {
		log.Fatal(err)
	}
	p.guard = g
	go p.run()

	var ix *Influx
	if b, err := os.ReadFile(*iToken); err != nil {
		log.Printf("history off: %v", err)
	} else {
		tok := strings.TrimSpace(string(b))
		tok = strings.TrimSpace(strings.TrimPrefix(tok, "token="))
		ix = newInflux(*iURL, *iOrg, *iBucket, tok, *name)
		go ix.run(p)
	}

	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, p.snapshot())
	})
	// POST /api/outlet {"n":1,"on":0}: JSON only, so that another site cannot send it from a browser
	mux.HandleFunc("POST /api/outlet", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			reply(w, 415, map[string]string{"error": "нужен application/json"})
			return
		}
		var q struct{ N, On *int }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&q); err != nil || q.N == nil || q.On == nil || (*q.On != 0 && *q.On != 1) {
			reply(w, 400, map[string]string{"error": `нужно {"n": номер, "on": 0 или 1}`})
			return
		}
		if err := p.switchOutlet(*q.N, *q.On, r.RemoteAddr); err != nil {
			code := 502
			if !p.guard.allowed(*q.N) {
				code = 403
			} else if strings.HasPrefix(err.Error(), "слишком часто") {
				code = 429
			}
			reply(w, code, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		if ix == nil {
			reply(w, 503, map[string]string{"error": "история выключена: нет токена InfluxDB"})
			return
		}
		h, err := ix.history(r.URL.Query().Get("what"), r.URL.Query().Get("n"), r.URL.Query().Get("range"))
		if err != nil {
			reply(w, 502, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, h)
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("donoff-pdu-view %s: device %s, page on %s, history %v, control %q (gap %s, %d an hour)", version, *host, *listen, ix != nil, *control, *minGap, *maxHour)
	log.Fatal(srv.ListenAndServe())
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
