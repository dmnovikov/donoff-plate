// donoff: a cooling / heating thermostat for the NanoPi: DS18B20 (1-Wire) -> fan relay (GPIO), with a web page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lab240/napi-donoff/internal/gpio"
	"github.com/lab240/napi-donoff/internal/mqtt"
	"github.com/lab240/napi-donoff/internal/sensor"
	"github.com/lab240/napi-donoff/internal/thermo"
	"github.com/lab240/napi-donoff/internal/web"
)

var version = "dev"

func main() {
	var (
		chip      = flag.String("chip", "/dev/gpiochip2", "GPIO chip of the fan relay")
		line      = flag.Int("line", 14, "GPIO line of the fan relay (GPIO2_B6 = 14)")
		activeLow = flag.Bool("active-low", false, "the relay is on at level 0")
		sensorID  = flag.String("sensor", "", "DS18B20 id, e.g. 28-0316b3f1abff (default: the first one found)")
		listen    = flag.String("listen", ":8080", "address of the web page and the API")
		trusted   = flag.String("trusted", "192.168.0.0/16", "networks that need no token (comma separated; loopback always)")
		token     = flag.String("token", "", "token for the other addresses (Authorization: Bearer)")
		tokenFile = flag.String("token-file", "", "file with the token (instead of --token)")
		state     = flag.String("state", "/var/lib/donoff/state.json", "file that keeps setpoint / hysteresis / mode")
		mqttState = flag.String("mqtt-state", "/var/lib/donoff/mqtt.json", "file that keeps the MQTT settings (set on the page /mqtt)")
		poll      = flag.Duration("poll", 2*time.Second, "sensor poll period")
		showVer   = flag.Bool("version", false, "print the version")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `donoff %s: thermostat with one relay.
  cool: T > setpoint -> on; T <= setpoint - hysteresis -> off; sensor error -> off.
  heat: T < setpoint -> on; T >= setpoint + hysteresis -> off; sensor error -> off.
Action (cool/heat), setpoint, hysteresis and mode (auto/on/off) are set on the web page.

`, version)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	log.SetFlags(0) // journald adds the time
	if err := run(*chip, *line, *activeLow, *sensorID, *listen, *trusted, *token, *tokenFile, *state, *mqttState, *poll); err != nil {
		log.Fatal(err)
	}
}

func run(chip string, line int, activeLow bool, sensorID, listen, trusted, token, tokenFile, state, mqttState string, poll time.Duration) error {
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return err
		}
		token = string(b)
	}
	token = strings.TrimSpace(token)
	nets, err := web.ParseNets(trusted)
	if err != nil {
		return err
	}

	relay, err := gpio.OpenOutput(chip, line, activeLow, "donoff-fan")
	if err != nil {
		return err
	}
	defer relay.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	th := thermo.New(&sensor.DS18B20{ID: sensorID}, relay, state, poll, 15*time.Second, 24*time.Hour)
	c := th.Status().Config
	log.Printf("donoff %s: relay %s (active %s), sensors %v, %s, setpoint %.1f, hysteresis %.1f, mode %s",
		version, relay, map[bool]string{false: "high", true: "low"}[activeLow], sensor.Find(), c.Action, c.Setpoint, c.Hysteresis, c.Mode)

	mq := mqtt.New(th, mqttState)

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: web.New(th, mq, token, nets, version).Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("web: %v", err)
			stop()
		}
	}()
	log.Printf("web page on %s (no token from loopback, %s; token %s for the others)", listen, trusted,
		map[bool]string{true: "set", false: "NOT set: refused"}[token != ""])

	done := make(chan struct{})
	go func() { th.Run(ctx); close(done) }()
	mq.Start()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	<-done
	mq.Stop()
	log.Printf("stopped, fan off")
	return nil
}
