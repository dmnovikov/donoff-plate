// donoff-panel: a web panel for the donoff devices on an MQTT broker (donoff-lib topics).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dmnovikov/donoff-plate/internal/panel"
)

var version = "dev"

func main() {
	creds := flag.String("creds", "mqtt.creds", "file with the broker: lines host, user, password [, port]")
	listen := flag.String("listen", ":8090", "address of the page")
	devices := flag.String("devices", "donoff-panel-devices.json", "file that keeps the last state of the devices over restarts (\"\" = none)")
	groups := flag.String("groups", "donoff-panel-groups.json", "file that keeps the groups of the devices (set on the page)")
	poll := flag.Duration("poll", time.Minute, "ask the devices with a relay for sh hotter / sh cooler / sh tls this often (0 = never)")
	influx := flag.String("influx", "influx.creds", "file with InfluxDB 2: lines url=, org=, bucket=, token= (\"\" = no history)")
	usersListen := flag.String("users-listen", "", "a second address of the page where everyone logs in (for a tunnel, e.g. 127.0.0.1:8091); a user sees only the devices of their groups")
	usersFile := flag.String("users", "donoff-panel-users.json", "file of the users (made on the first start with the user admin and its invite link in the log; \"\" = no users)")
	trusted := flag.String("trusted", "192.168.0.0/16", "networks that may open the page and send commands (loopback always)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "donoff-panel %s: devices of /<user>/+/out/#, commands to /<user>/<dev_id>/in/params\n\n", version)
		flag.PrintDefaults()
	}
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	b, err := os.ReadFile(*creds)
	if err != nil {
		log.Fatal(err)
	}
	c, err := panel.ParseCreds(string(b))
	if err != nil {
		log.Fatalf("%s: %v", *creds, err)
	}
	nets, err := panel.ParseNets(*trusted)
	if err != nil {
		log.Fatal(err)
	}

	h := panel.New(c)
	stopKeep := make(chan struct{})
	keepDone := make(chan struct{})
	if *devices != "" {
		if err := h.LoadDevices(*devices); err != nil {
			log.Fatal(err)
		}
		go func() { h.KeepDevices(*devices, time.Minute, stopKeep); close(keepDone) }()
	} else {
		close(keepDone)
	}
	if err := h.LoadGroups(*groups); err != nil {
		log.Fatal(err)
	}
	stopInflux := make(chan struct{})
	influxDone := make(chan struct{})
	close(influxDone)
	if *influx != "" {
		b, err := os.ReadFile(*influx)
		if err != nil {
			log.Fatal(err)
		}
		x, err := panel.ParseInflux(string(b))
		if err != nil {
			log.Fatalf("%s: %v", *influx, err)
		}
		h.SetInflux(x)
		if err := h.Seed(x); err != nil {
			log.Printf("history: the last values of the silent devices: %v", err)
		}
		influxDone = make(chan struct{})
		go func() { x.Run(stopInflux); close(influxDone) }()
		log.Printf("history: InfluxDB %s, org %s, bucket %s", x.URL, x.Org, x.Bucket)
	}
	h.Start()
	stopPoll := make(chan struct{})
	go h.Poll(*poll, stopPoll)
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second}
	var users *panel.Users
	if *usersFile != "" {
		u, invite, err := panel.LoadUsers(*usersFile)
		if err != nil {
			log.Fatal(err)
		}
		users = u
		if invite != "" {
			log.Printf("users: made %s with the user admin; set its password at <the page>/invite?t=%s (valid 7 days)", *usersFile, invite)
		}
	}
	srv.Handler = h.Handler(nets, users)
	var tsrv *http.Server
	if *usersListen != "" {
		if users == nil {
			log.Fatal("--users-listen needs --users")
		}
		tsrv = &http.Server{Addr: *usersListen, Handler: h.UsersHandler(users), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := tsrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}()
		log.Printf("page with logins on %s", *usersListen)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		if tsrv != nil {
			tsrv.Shutdown(sctx)
		}
	}()
	log.Printf("donoff-panel %s: broker %s:%d as %s, page on %s (trusted %s), poll %v", version, c.Host, c.Port, c.User, *listen, *trusted, *poll)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	close(stopPoll)
	h.Stop()
	close(stopKeep)
	<-keepDone
	close(stopInflux)
	<-influxDone // the last points
}
