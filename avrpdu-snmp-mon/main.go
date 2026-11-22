// donoff-pdu-view: a page for ATS and PDU devices by SNMP (each model by a profile: RCNTEC RPCM
// and Elemy ATS-1204 built in). It polls each device by SNMP GET, shows the devices as tiles on the
// main page and each one's inputs and outlets on its own page, and keeps a small history in
// InfluxDB 2 for the charts (no InfluxDB: tiles only). One device by the flags, or many by -config.
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
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var version = "0.2.0"

//go:embed web
var webFS embed.FS

// Config: the file of -config.
type Config struct {
	Listen   string `yaml:"listen"`
	Profiles string `yaml:"profiles"`
	Groups   string `yaml:"groups_file"` // the groups of the tabs (JSON); default: $STATE_DIRECTORY/groups.json
	Influx   struct {
		URL       string `yaml:"url"`
		Org       string `yaml:"org"`
		Bucket    string `yaml:"bucket"`
		TokenFile string `yaml:"token_file"`
	} `yaml:"influx"`
	Devices   []DevConf   `yaml:"devices"`
	Drills    []DrillConf `yaml:"drills"`     // the automations: UPS drills (drill.go)
	DrillsLog string      `yaml:"drills_log"` // the journal (JSON); default: $STATE_DIRECTORY/drills.json
}

type groupReq struct {
	Dev, Group, From, To, Name string
	Order                      []string
}

type DevConf struct {
	ID        string        `yaml:"id"`                   // the name in the address and in InfluxDB (tag dev)
	Host      string        `yaml:"host"`                 // host or host:port
	Community string        `yaml:"community"`            // public by default
	Profile   string        `yaml:"profile"`              // auto by default
	Main      int           `yaml:"main"`                 // the main input, 1 by default
	Control   string        `yaml:"control"`              // the outlets the page may switch: "1" or "1,3"
	MinGap    time.Duration `yaml:"min_gap"`              // 10s by default
	MaxHour   int           `yaml:"max_hour"`             // 20 by default
	Kind      string        `yaml:"kind"`                 // the kind on the page instead of the profile's: PDU (ПДУ), ATS (АВР), CCU (Контроллер шкафа)
	WriteFile string        `yaml:"write_community_file"` // the SET community in this file (not in the config); else community
}

type device struct {
	conf DevConf
	p    *poller
	ix   *Influx
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func main() {
	var (
		cfgFile   = flag.String("config", "", "a YAML file with the devices (see README); without it one device by the flags below")
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

	var cfg Config
	if *cfgFile != "" {
		b, err := os.ReadFile(*cfgFile)
		if err != nil {
			log.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			log.Fatalf("%s: %v", *cfgFile, err)
		}
	} else if *host != "" {
		cfg.Devices = []DevConf{{ID: *name, Host: *host, Community: *community, Profile: *profID, Main: *mainIn,
			Control: *control, MinGap: *minGap, MaxHour: *maxHour}}
	}
	// the flags fill what the file leaves out
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&cfg.Listen, *listen)
	def(&cfg.Profiles, *profDir)
	def(&cfg.Influx.URL, *iURL)
	def(&cfg.Influx.Org, *iOrg)
	def(&cfg.Influx.Bucket, *iBucket)
	def(&cfg.Influx.TokenFile, *iToken)
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(int64(16+8*len(cfg.Devices)) << 20) // a small board: collect early
	}

	all, err := loadProfiles(cfg.Profiles)
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
	if len(cfg.Devices) == 0 {
		log.Fatal("no devices: give -config devices.yaml or -host 192.168.0.78")
	}

	token := ""
	if b, err := os.ReadFile(cfg.Influx.TokenFile); err != nil {
		log.Printf("history off: %v", err)
	} else {
		token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "token="))
	}

	devs := map[string]*device{}
	var order []string
	for _, c := range cfg.Devices {
		if !idRe.MatchString(c.ID) || c.Host == "" {
			log.Fatalf("device %q: id (letters, digits, - and _) and host are needed", c.ID)
		}
		if devs[c.ID] != nil {
			log.Fatalf("device %q twice", c.ID)
		}
		def(&c.Community, "public")
		def(&c.Profile, "auto")
		if c.Main == 0 {
			c.Main = 1
		}
		if c.MinGap == 0 {
			c.MinGap = 10 * time.Second
		}
		if c.MaxHour == 0 {
			c.MaxHour = 20
		}
		var prof *Profile
		if c.Profile != "auto" {
			if prof = all[c.Profile]; prof == nil {
				log.Fatalf("device %s: no profile %q (there are: %s)", c.ID, c.Profile, profileIDs(all))
			}
		}
		d := &device{conf: c, p: newPoller(c.ID, c.Host, c.Community, c.Main, prof, all)}
		d.p.kind = c.Kind
		if d.p.guard, err = newGuard(c.Control, c.MinGap, max(c.MaxHour, 1)); err != nil {
			log.Fatalf("device %s: %v", c.ID, err)
		}
		if c.WriteFile != "" {
			b, err := os.ReadFile(c.WriteFile)
			if err != nil {
				log.Fatalf("device %s: %v", c.ID, err)
			}
			if err := d.p.writeWith(strings.TrimSpace(string(b))); err != nil {
				log.Fatalf("device %s: %v", c.ID, err)
			}
		}
		go d.p.run()
		if token != "" {
			d.ix = newInflux(cfg.Influx.URL, cfg.Influx.Org, cfg.Influx.Bucket, token, c.ID)
			go d.ix.run(d.p)
		}
		devs[c.ID] = d
		order = append(order, c.ID)
		log.Printf("device %s: %s, profile %s, control %q (gap %s, %d an hour)", c.ID, c.Host, c.Profile, c.Control, c.MinGap, c.MaxHour)
	}
	// the device of a request: ?dev=, or the only one
	pick := func(w http.ResponseWriter, id string) *device {
		if id == "" && len(order) == 1 {
			id = order[0]
		}
		d := devs[id]
		if d == nil {
			reply(w, 404, map[string]string{"error": "нет устройства " + id})
		}
		return d
	}

	if cfg.Groups == "" && os.Getenv("STATE_DIRECTORY") != "" {
		cfg.Groups = strings.Split(os.Getenv("STATE_DIRECTORY"), ":")[0] + "/groups.json"
	}
	grp, err := loadGroups(cfg.Groups)
	if err != nil {
		log.Fatal(err)
	}

	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	// who looks: the public site (nginx with logins) gives X-Pdu-User and X-Pdu-Role (admin or view);
	// without them (the page opened in the LAN) everything is allowed, as before
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]string{"user": r.Header.Get("X-Pdu-User"), "role": role(r)})
	})
	// the automation tab: only for admin (a "view" user gets 403 on the page and on its API)
	if cfg.DrillsLog == "" && os.Getenv("STATE_DIRECTORY") != "" {
		cfg.DrillsLog = strings.Split(os.Getenv("STATE_DIRECTORY"), ":")[0] + "/drills.json"
	}
	dlog, err := loadDrillLog(cfg.DrillsLog)
	if err != nil {
		log.Fatal(err)
	}
	drills := map[string]*drill{}
	var drillOrder []string
	for _, c := range cfg.Drills {
		dr, err := newDrill(c, devs, dlog)
		if err != nil {
			log.Fatal(err)
		}
		if drills[c.ID] != nil {
			log.Fatalf("drill %q twice", c.ID)
		}
		drills[c.ID] = dr
		drillOrder = append(drillOrder, c.ID)
		go dr.recover()
		log.Printf("drill %s: ats %s, start %s/%d, ups %s/%d, limit %s", c.ID, c.ATS, c.Start.Dev, c.Start.Outlet, c.UPS.Dev, c.UPS.Outlet, dr.conf.Max)
	}
	mux.Handle("GET /automation.html", adminOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, sub, "automation.html")
	})))
	mux.Handle("GET /api/drills", adminOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type view struct {
			DrillConf
			MaxMin  float64   `json:"max_min"`
			Why     []string  `json:"why"`
			Run     *DrillRun `json:"run"`
			Inputs  []Input   `json:"inputs"`
			StartOn *int      `json:"start_on"`
			UPSOn   *int      `json:"ups_on"`
		}
		out := []view{}
		for _, id := range drillOrder {
			dr := drills[id]
			v := view{DrillConf: dr.conf, MaxMin: dr.conf.Max.Minutes(), Why: dr.checks(), Run: dr.current()}
			v.Inputs = dr.ats.p.snapshot().Inputs
			if o := outletOf(dr.sw.p.snapshot(), dr.conf.Start.Outlet); o != nil {
				v.StartOn = &o.On
			}
			if o := outletOf(dr.ups.p.snapshot(), dr.conf.UPS.Outlet); o != nil {
				v.UPSOn = &o.On
			}
			out = append(out, v)
		}
		reply(w, 200, map[string]any{"drills": out, "log": dlog.list(), "now": time.Now()})
	})))
	drillAPI := func(path string, do func(dr *drill, q struct {
		ID     string
		MaxMin float64
	}, who string) (any, error)) {
		mux.Handle(path, adminOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				reply(w, 415, map[string]string{"error": "нужен application/json"})
				return
			}
			var q struct {
				ID     string
				MaxMin float64 `json:"max_min"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&q); err != nil {
				reply(w, 400, map[string]string{"error": "плохой JSON"})
				return
			}
			dr := drills[q.ID]
			if dr == nil {
				reply(w, 404, map[string]string{"error": "нет тренировки " + q.ID})
				return
			}
			v, err := do(dr, struct {
				ID     string
				MaxMin float64
			}{q.ID, q.MaxMin}, who(r))
			if err != nil {
				reply(w, 409, map[string]string{"error": err.Error()})
				return
			}
			reply(w, 200, v)
		})))
	}
	drillAPI("POST /api/drill/start", func(dr *drill, q struct {
		ID     string
		MaxMin float64
	}, who string) (any, error) {
		return dr.begin(who, q.MaxMin)
	})
	drillAPI("POST /api/drill/stop", func(dr *drill, q struct {
		ID     string
		MaxMin float64
	}, who string) (any, error) {
		return map[string]bool{"ok": true}, dr.halt(who)
	})
	// all the devices for the main page, in the order of the config
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		out := make([]State, 0, len(order))
		for _, id := range order {
			out = append(out, devs[id].p.snapshot())
		}
		gm, gord := grp.view()
		reply(w, 200, map[string]any{"devices": out, "history": token != "", "version": version, "groups": gm, "group_order": gord})
	})
	// the groups (tabs) of the main page: JSON only, as /api/outlet
	groupAPI := func(path string, do func(q groupReq) error) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				reply(w, 415, map[string]string{"error": "нужен application/json"})
				return
			}
			var q groupReq
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&q); err != nil {
				reply(w, 400, map[string]string{"error": "плохой JSON"})
				return
			}
			if err := do(q); err != nil {
				reply(w, 400, map[string]string{"error": err.Error()})
				return
			}
			reply(w, 200, map[string]any{"ok": true})
		})
	}
	groupAPI("PUT /api/group", func(q groupReq) error { // {"dev": "ccu1", "group": "Шкаф 1"} ("" = no group)
		if devs[q.Dev] == nil {
			return fmt.Errorf("нет устройства %s", q.Dev)
		}
		return grp.set(q.Dev, q.Group)
	})
	groupAPI("POST /api/group/rename", func(q groupReq) error { return grp.rename(q.From, q.To) }) // {"from", "to"}
	groupAPI("POST /api/group/delete", func(q groupReq) error { return grp.remove(q.Name) })       // {"name"}
	groupAPI("PUT /api/group/order", func(q groupReq) error { return grp.setOrder(q.Order) })      // {"order": [...]}
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		if d := pick(w, r.URL.Query().Get("dev")); d != nil {
			reply(w, 200, d.p.snapshot())
		}
	})
	// POST /api/outlet {"dev":"rpcm1","n":1,"on":0}: JSON only, so that another site cannot send it from a browser
	mux.HandleFunc("POST /api/outlet", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			reply(w, 415, map[string]string{"error": "нужен application/json"})
			return
		}
		var q struct {
			Dev   string
			N, On *int
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&q); err != nil || q.N == nil || q.On == nil || (*q.On != 0 && *q.On != 1) {
			reply(w, 400, map[string]string{"error": `нужно {"dev": "устройство", "n": номер, "on": 0 или 1}`})
			return
		}
		d := pick(w, q.Dev)
		if d == nil {
			return
		}
		if err := d.p.switchOutlet(*q.N, *q.On, who(r), false); err != nil {
			code := 502
			if !d.p.guard.allowed(*q.N) {
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
		d := pick(w, r.URL.Query().Get("dev"))
		if d == nil {
			return
		}
		if d.ix == nil {
			reply(w, 503, map[string]string{"error": "история выключена: нет токена InfluxDB"})
			return
		}
		h, err := d.ix.history(r.URL.Query().Get("what"), r.URL.Query().Get("n"), r.URL.Query().Get("range"))
		if err != nil {
			reply(w, 502, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, h)
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: viewOnly(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("donoff-pdu-view %s: %d devices, page on %s, history %v", version, len(order), cfg.Listen, token != "")
	log.Fatal(srv.ListenAndServe())
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// role: "view" or "admin" from the public site; "admin" without the header (the LAN).
func role(r *http.Request) string {
	if r.Header.Get("X-Pdu-Role") == "view" {
		return "view"
	}
	return "admin"
}

// who: the login of the public site, else the address.
func who(r *http.Request) string {
	if u := r.Header.Get("X-Pdu-User"); u != "" {
		return u
	}
	return r.RemoteAddr
}

// adminOnly: the page or API is for admin only, even to read.
func adminOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role(r) != "admin" {
			reply(w, 403, map[string]string{"error": "только для admin"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// viewOnly: a "view" user only reads (nginx refuses the writes too; this is the second lock).
func viewOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role(r) == "view" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			reply(w, 403, map[string]string{"error": "нет прав на изменение: войдите как admin"})
			return
		}
		h.ServeHTTP(w, r)
	})
}
