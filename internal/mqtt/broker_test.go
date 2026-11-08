package mqtt

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/lab240/napi-donoff/internal/thermo"
)

// TestBroker needs a real broker: DONOFF_MQTT_TEST=host:port go test ./internal/mqtt -run Broker -v
func TestBroker(t *testing.T) {
	addr := os.Getenv("DONOFF_MQTT_TEST")
	if addr == "" {
		t.Skip("DONOFF_MQTT_TEST=host:port is not set")
	}
	host, ps, _ := strings.Cut(addr, ":")
	port, _ := strconv.Atoi(ps)

	th := thermo.New(sens{}, rel{}, "", 50*time.Millisecond, time.Second, time.Hour)
	c := New(th, filepath.Join(t.TempDir(), "mqtt.json"))
	if err := c.SetConfig(Config{Enabled: true, Server: host, Port: port, User: "test", Password: "x", DevID: "napi1", Interval: 1}); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	var mu sync.Mutex
	got := map[string][]string{}
	sub := paho.NewClient(paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("donoff-test-sub"))
	if tk := sub.Connect(); tk.Wait() && tk.Error() != nil {
		t.Fatal(tk.Error())
	}
	defer sub.Disconnect(100)
	sub.Subscribe("/test/napi1/#", 0, func(_ paho.Client, m paho.Message) {
		mu.Lock()
		got[m.Topic()] = append(got[m.Topic()], string(m.Payload()))
		mu.Unlock()
	}).Wait()

	deadline := time.Now().Add(10 * time.Second)
	for !c.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !c.Status().Connected {
		t.Fatalf("not connected: %+v", c.Status())
	}
	go th.Run(t.Context())
	time.Sleep(2500 * time.Millisecond)

	for _, cmd := range []string{"sh ctempl", "ctempl=2700", "tdelta=150", "r11"} {
		sub.Publish("/test/napi1/in/params", 0, false, cmd).Wait()
		time.Sleep(1200 * time.Millisecond) // > the 1 s anti flood
	}
	sub.Publish("/test/napi1/in/params", 0, false, "sh r1").Wait()
	sub.Publish("/test/napi1/in/params", 0, false, "sh r1").Wait() // too fast: DDOS CHECK
	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for topic, v := range got {
		t.Logf("%-28s %q", topic, v)
	}
	want := map[string]string{
		"/test/napi1/out/sensors/temp_out": "23.50",
		"/test/napi1/out/relays/r1":       "1",
		"/test/napi1/out/info":            "DDOS CHECK",
	}
	for topic, w := range want {
		if !contains(got[topic], w) {
			t.Errorf("%s: no %q in %q", topic, w, got[topic])
		}
	}
	for _, w := range []string{"ctempl=2600", "ctempl=2700", "tdelta=150", "r1=1"} {
		if !contains(got["/test/napi1/out/info"], w) {
			t.Errorf("/out/info: no %q in %q", w, got["/test/napi1/out/info"])
		}
	}
	if len(got["/test/napi1/out/time_up"]) == 0 || len(got["/test/napi1/out/log"]) == 0 {
		t.Error("no time_up or log")
	}
	if s := th.Status(); s.Setpoint != 27 || s.Hysteresis != 1.5 || s.Mode != thermo.On {
		t.Errorf("config: %+v", s.Config)
	}
}

func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
