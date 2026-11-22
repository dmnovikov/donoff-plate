package main

// The UPS drill (тренировка ИБП), the first automation. The operator presses start: if the start
// outlet is off and both inputs of the ATS work, the program turns the start outlet on (it stands
// for the contactor that cuts the UPS mains) and watches the ATS every second. When an input of
// the ATS is lost (the UPS is empty; on the bench the operator turns the "UPS" outlet off by hand)
// the time from the start is the result. Then, and after any failure, the program puts the outlets
// back: the start outlet off, the UPS outlet on. Failed: the start outlet went off by someone
// else, the operator pressed stop, no answer from the ATS or the outlet, or the time limit.
// Every drill goes to a journal (a JSON file).

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type OutletRef struct {
	Dev    string `yaml:"dev" json:"dev"`
	Outlet int    `yaml:"outlet" json:"outlet"`
}

type DrillConf struct {
	ID    string        `yaml:"id" json:"id"`
	Title string        `yaml:"title" json:"title"`
	ATS   string        `yaml:"ats" json:"ats"`     // the device whose inputs are watched
	Start OutletRef     `yaml:"start" json:"start"` // on: the drill starts
	UPS   OutletRef     `yaml:"ups" json:"ups"`     // turned on again at the end (the UPS lives again)
	Max   time.Duration `yaml:"max" json:"-"`       // the time limit by default, 60m
}

// DrillRun: one drill in the journal.
type DrillRun struct {
	N       int        `json:"n"`
	Drill   string     `json:"drill"`
	Title   string     `json:"title"`
	By      string     `json:"by"`
	Start   time.Time  `json:"start"`
	End     *time.Time `json:"end,omitempty"`
	Seconds float64    `json:"seconds"`
	MaxMin  float64    `json:"max_min"`
	Result  string     `json:"result"` // running, ok, fail, timeout
	Reason  string     `json:"reason"`
	Input   int        `json:"input,omitempty"` // the input that was lost
	Restore []string   `json:"restore"`         // what was done at the end
}

type drillLog struct {
	mu   sync.Mutex
	path string
	runs []DrillRun
}

const drillKeep = 500

func loadDrillLog(path string) (*drillLog, error) {
	l := &drillLog{path: path}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &l.runs); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return l, nil
}

// put adds or replaces (by N) a run and writes the file.
func (l *drillLog) put(r DrillRun) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i := slices.IndexFunc(l.runs, func(x DrillRun) bool { return x.N == r.N }); i >= 0 {
		l.runs[i] = r
	} else {
		l.runs = append(l.runs, r)
		if len(l.runs) > drillKeep {
			l.runs = l.runs[len(l.runs)-drillKeep:]
		}
	}
	if l.path == "" {
		return
	}
	b, _ := json.MarshalIndent(l.runs, "", " ")
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		log.Printf("drill journal: %v", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		log.Printf("drill journal: %v", err)
	}
}

func (l *drillLog) next() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.runs {
		n = max(n, r.N)
	}
	return n + 1
}

func (l *drillLog) list() []DrillRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]DrillRun{}, l.runs...)
	slices.Reverse(out)
	return out
}

type drill struct {
	conf         DrillConf
	ats, sw, ups *device
	log          *drillLog
	mu           sync.Mutex
	run          *DrillRun
	stop         chan string
}

func newDrill(c DrillConf, devs map[string]*device, l *drillLog) (*drill, error) {
	d := &drill{conf: c, ats: devs[c.ATS], sw: devs[c.Start.Dev], ups: devs[c.UPS.Dev], log: l}
	if !idRe.MatchString(c.ID) {
		return nil, fmt.Errorf("drill %q: id letters, digits, - and _", c.ID)
	}
	if d.ats == nil || d.sw == nil || d.ups == nil {
		return nil, fmt.Errorf("drill %s: no device ats %q / start %q / ups %q", c.ID, c.ATS, c.Start.Dev, c.UPS.Dev)
	}
	if !d.sw.p.guard.allowed(c.Start.Outlet) || !d.ups.p.guard.allowed(c.UPS.Outlet) {
		return nil, fmt.Errorf("drill %s: outlet %s/%d and %s/%d must be in the control of their devices", c.ID, c.Start.Dev, c.Start.Outlet, c.UPS.Dev, c.UPS.Outlet)
	}
	if d.conf.Max <= 0 {
		d.conf.Max = time.Hour
	}
	if d.conf.Title == "" {
		d.conf.Title = "Тренировка ИБП"
	}
	return d, nil
}

func outletOf(s State, n int) *Outlet {
	for i := range s.Outlets {
		if s.Outlets[i].N == n {
			return &s.Outlets[i]
		}
	}
	return nil
}

// inputOK: the voltage is there, in the limits, no alarms.
func inputOK(in Input) bool {
	return in.Present == 1 && len(in.Alarms) == 0 && (in.VMin <= 0 || in.V >= in.VMin) && (in.VMax <= 0 || in.V <= in.VMax)
}

func inputLost(in Input) bool { return in.Present == 0 || (in.VMin > 0 && in.V < in.VMin) }

// checks: why the drill may not start now (empty: it may).
func (d *drill) checks() []string {
	why := []string{}
	a, s, u := d.ats.p.snapshot(), d.sw.p.snapshot(), d.ups.p.snapshot()
	if !a.OK || time.Since(a.At) > 10*time.Second {
		why = append(why, "нет связи с "+d.conf.ATS)
	} else {
		if len(a.Inputs) < 2 {
			why = append(why, d.conf.ATS+": меньше двух вводов")
		}
		for _, in := range a.Inputs {
			if !inputOK(in) {
				why = append(why, fmt.Sprintf("ввод %d не в норме", in.N))
			}
		}
	}
	if o := outletOf(s, d.conf.Start.Outlet); !s.OK || o == nil || time.Since(s.OutAt) > 15*time.Second {
		why = append(why, fmt.Sprintf("нет данных о розетке %d %s", d.conf.Start.Outlet, d.conf.Start.Dev))
	} else if o.On == 1 || o.Admin == 1 {
		why = append(why, fmt.Sprintf("розетка %d %s уже включена", d.conf.Start.Outlet, d.conf.Start.Dev))
	}
	if !u.OK || outletOf(u, d.conf.UPS.Outlet) == nil {
		why = append(why, "нет связи с "+d.conf.UPS.Dev+": некому вернуть розетку "+fmt.Sprint(d.conf.UPS.Outlet))
	}
	return why
}

func (d *drill) current() *DrillRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.run == nil {
		return nil
	}
	r := *d.run
	return &r
}

// begin: the checks on fresh readings, the start outlet on, then the watch in the background.
func (d *drill) begin(who string, maxMin float64) (*DrillRun, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.run != nil {
		return nil, errors.New("тренировка уже идёт")
	}
	limit := d.conf.Max
	if maxMin > 0 {
		if maxMin > 24*60 {
			return nil, errors.New("предел не больше 24 часов")
		}
		limit = time.Duration(maxMin * float64(time.Minute))
	}
	if d.ats.p.prof == nil || d.sw.p.prof == nil || d.ups.p.prof == nil {
		return nil, errors.New("не начинаю: устройства ещё не опознаны, подождите")
	}
	d.ats.p.fast()
	d.sw.p.outlets()
	if why := d.checks(); len(why) > 0 {
		return nil, errors.New("не начинаю: " + strings.Join(why, "; "))
	}
	if err := d.sw.p.switchOutlet(d.conf.Start.Outlet, 1, "drill "+d.conf.ID+" ("+who+")", true); err != nil {
		return nil, fmt.Errorf("не включилась розетка %d %s: %v", d.conf.Start.Outlet, d.conf.Start.Dev, err)
	}
	r := &DrillRun{N: d.log.next(), Drill: d.conf.ID, Title: d.conf.Title, By: who, Start: time.Now(), MaxMin: limit.Minutes(), Result: "running", Restore: []string{}}
	d.run, d.stop = r, make(chan string, 1)
	d.log.put(*r)
	log.Printf("drill %s #%d: started by %s, limit %s", d.conf.ID, r.N, who, limit)
	go d.watch(r, limit, d.stop)
	return r, nil
}

// halt: the operator's stop.
func (d *drill) halt(who string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.run == nil {
		return errors.New("тренировка не идёт")
	}
	select {
	case d.stop <- "остановлено оператором (" + who + ")":
	default:
	}
	return nil
}

func (d *drill) watch(r *DrillRun, limit time.Duration, stop chan string) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	t0 := r.Start
	result, reason := "", ""
	for result == "" {
		select {
		case why := <-stop:
			result, reason = "fail", why
			continue
		case <-t.C:
		}
		now := time.Now()
		if err := d.ats.p.fast(); err != nil {
			result, reason = "fail", "нет связи с "+d.conf.ATS+": "+err.Error()
			break
		}
		a := d.ats.p.snapshot()
		for _, in := range a.Inputs {
			if inputLost(in) {
				result, reason, r.Input = "ok", fmt.Sprintf("пропал ввод %d", in.N), in.N
				break
			}
		}
		d.mu.Lock()
		r.Seconds = now.Sub(t0).Seconds()
		d.mu.Unlock()
		if result != "" {
			break
		}
		if err := d.sw.p.outlets(); err != nil {
			result, reason = "fail", "нет связи с "+d.conf.Start.Dev+": "+err.Error()
			break
		}
		if o := outletOf(d.sw.p.snapshot(), d.conf.Start.Outlet); o == nil || o.Admin != 1 {
			result, reason = "fail", fmt.Sprintf("розетку %d %s выключили не из тренировки", d.conf.Start.Outlet, d.conf.Start.Dev)
			break
		}
		if now.Sub(t0) >= limit {
			result, reason = "timeout", fmt.Sprintf("превышен предел %s: ввод не пропал", limit)
		}
	}
	end := time.Now()
	d.mu.Lock()
	r.Result, r.Reason, r.End = result, reason, &end
	if result != "ok" {
		r.Seconds = end.Sub(t0).Seconds()
	}
	d.mu.Unlock()
	log.Printf("drill %s #%d: %s, %.1f s, %s", d.conf.ID, r.N, result, r.Seconds, reason)
	d.log.put(*r)
	d.restore(r)
	d.mu.Lock()
	d.run = nil
	d.mu.Unlock()
}

// restore puts the outlets back: the start outlet off, the UPS outlet on; a few tries each.
func (d *drill) restore(r *DrillRun) {
	set := func(dev *device, ref OutletRef, v int) string {
		word := map[int]string{0: "выключена", 1: "включена"}[v]
		var err error
		for try := 0; try < 3; try++ {
			if try > 0 {
				time.Sleep(2 * time.Second)
			}
			if dev.p.prof == nil {
				err = errors.New("устройство не опознано")
				continue
			}
			if err = dev.p.outlets(); err == nil {
				if o := outletOf(dev.p.snapshot(), ref.Outlet); o != nil && o.Admin == v && o.On == v {
					return fmt.Sprintf("розетка %d %s %s", ref.Outlet, ref.Dev, word)
				}
			}
			if err = dev.p.switchOutlet(ref.Outlet, v, "drill "+d.conf.ID+" restore", true); err == nil {
				return fmt.Sprintf("розетка %d %s %s", ref.Outlet, ref.Dev, word)
			}
		}
		return fmt.Sprintf("ВНИМАНИЕ: розетка %d %s не %s: %v", ref.Outlet, ref.Dev, word, err)
	}
	done := []string{set(d.sw, d.conf.Start, 0), set(d.ups, d.conf.UPS, 1)}
	d.mu.Lock()
	r.Restore = done
	d.mu.Unlock()
	for _, s := range done {
		log.Printf("drill %s #%d: %s", d.conf.ID, r.N, s)
	}
	d.log.put(*r)
}

// recover: a drill left "running" by a restart of the program is failed, and the outlets go back.
func (d *drill) recover() {
	for _, r := range d.log.list() {
		if r.Drill != d.conf.ID || r.Result != "running" {
			continue
		}
		for i := 0; i < 60 && (d.sw.p.prof == nil || d.ups.p.prof == nil); i++ {
			time.Sleep(time.Second)
		}
		end := time.Now()
		r.Result, r.Reason, r.End = "fail", "прервано перезапуском программы", &end
		r.Seconds = end.Sub(r.Start).Seconds()
		d.log.put(r)
		d.restore(&r)
	}
}
