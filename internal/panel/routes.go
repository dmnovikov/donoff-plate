package panel

import (
	_ "embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

//go:embed web/login.html
var loginHTML []byte

//go:embed web/admin.html
var adminHTML []byte

type whoKey struct{}

func whoOf(r *http.Request) *Who { w, _ := r.Context().Value(whoKey{}).(*Who); return w }

func html(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	}
}

func bad(w http.ResponseWriter, code int, msg string) { reply(w, code, map[string]string{"error": msg}) }

func decode(r *http.Request, v any) bool {
	return json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(v) == nil
}

// routes: the pages and the API; whoOf(r) is set by the listener (nil only on the open paths).
//
//	GET  /login /invite /admin          pages
//	POST /login                         form name, password -> session cookie
//	POST /logout
//	POST /api/v1/invite                 {"token", "password"}: set the password by the invite link
//	GET  /api/v1/users                  admin: users and groups
//	POST /api/v1/users                  admin: {"name", "admin", "control", "groups"} -> {"invite": token}
//	PUT  /api/v1/users/{name}           admin: {"admin", "control", "groups"}
//	POST /api/v1/users/{name}/invite    admin: a new invite (set / reset the password) -> {"invite": token}
//	POST /api/v1/users/{name}/logout    admin: end every session of the user
//	DELETE /api/v1/users/{name}         admin
func (h *Hub) routes(u *Users) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", html(page))
	m.HandleFunc("GET /history", html(historyPage))
	m.HandleFunc("GET /login", html(loginHTML))
	m.HandleFunc("GET /invite", html(loginHTML))
	m.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		if who := whoOf(r); who == nil || !who.Admin || u == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		html(adminHTML)(w, r)
	})

	m.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		v := h.snapshotFor(whoOf(r))
		v.Users = u != nil
		reply(w, 200, v)
	})
	m.HandleFunc("GET /api/v1/history", func(w http.ResponseWriter, r *http.Request) {
		dev := r.URL.Query().Get("dev")
		if who := whoOf(r); who == nil || !who.CanSee(h.GroupOf(dev)) {
			bad(w, 403, "no such device for you")
			return
		}
		if h.influx == nil {
			bad(w, 404, "the panel runs without --influx")
			return
		}
		x, err := h.influx.History(dev, r.URL.Query().Get("range"))
		if err != nil {
			bad(w, 400, err.Error())
			return
		}
		reply(w, 200, x)
	})
	m.HandleFunc("PUT /api/v1/group", func(w http.ResponseWriter, r *http.Request) {
		if who := whoOf(r); who == nil || !who.Admin {
			bad(w, 403, "only the admin puts devices into groups")
			return
		}
		var in struct{ Dev, Group string }
		if !decode(r, &in) {
			bad(w, 400, "bad JSON")
			return
		}
		if err := h.SetGroup(in.Dev, in.Group); err != nil {
			bad(w, 400, err.Error())
			return
		}
		reply(w, 200, map[string]string{"ok": "saved"})
	})
	m.HandleFunc("POST /api/v1/cmd", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Dev, Cmd string }
		if !decode(r, &in) {
			bad(w, 400, "bad JSON")
			return
		}
		who := whoOf(r)
		switch {
		case who == nil || !who.CanSee(h.GroupOf(in.Dev)):
			bad(w, 403, "no such device for you")
			return
		case !who.Control && !strings.HasPrefix(strings.TrimSpace(in.Cmd), "sh "):
			bad(w, 403, "you may only look (sh ...) — ask the admin for control")
			return
		}
		if err := h.Send(in.Dev, in.Cmd); err != nil {
			bad(w, 400, err.Error())
			return
		}
		log.Printf("cmd from %s (%s)", r.RemoteAddr, who.Name)
		reply(w, 200, map[string]string{"ok": "sent"})
	})

	if u == nil {
		return m
	}
	m.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		name, pass := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("password")
		if err := u.Login(name, pass); err != nil {
			log.Printf("login %q from %s: %v", name, r.RemoteAddr, err)
			http.Redirect(w, r, "/login?e=1", http.StatusSeeOther)
			return
		}
		setSession(w, u.Session(name), int(sessionTTL.Seconds()))
		log.Printf("login %q from %s", name, r.RemoteAddr)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	m.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		setSession(w, "", -1)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	m.HandleFunc("GET /api/v1/invite", func(w http.ResponseWriter, r *http.Request) {
		name := u.InviteUser(r.URL.Query().Get("t"))
		if name == "" {
			bad(w, 404, "the link is not valid any more: ask the admin for a new one")
			return
		}
		reply(w, 200, map[string]string{"name": name})
	})
	m.HandleFunc("POST /api/v1/invite", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Token, Password string }
		if !decode(r, &in) {
			bad(w, 400, "bad JSON")
			return
		}
		name, err := u.AcceptInvite(in.Token, in.Password)
		if err != nil {
			bad(w, 400, err.Error())
			return
		}
		setSession(w, u.Session(name), int(sessionTTL.Seconds()))
		log.Printf("invite accepted: %q from %s", name, r.RemoteAddr)
		reply(w, 200, map[string]string{"ok": "password set", "name": name})
	})

	admin := func(f http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if who := whoOf(r); who == nil || !who.Admin {
				bad(w, 403, "only the admin")
				return
			}
			f(w, r)
		}
	}
	type userIn struct {
		Name           string
		Admin, Control bool
		Groups         []string
	}
	done := func(w http.ResponseWriter, r *http.Request, what string, err error, out map[string]string) {
		if err != nil {
			bad(w, 400, err.Error())
			return
		}
		log.Printf("users: %s by %s", what, whoOf(r).Name)
		if out == nil {
			out = map[string]string{"ok": "saved"}
		}
		reply(w, 200, out)
	}
	m.HandleFunc("GET /api/v1/users", admin(func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"users": u.Views(), "groups": h.AllGroups(), "me": whoOf(r)})
	}))
	m.HandleFunc("POST /api/v1/users", admin(func(w http.ResponseWriter, r *http.Request) {
		var in userIn
		if !decode(r, &in) {
			bad(w, 400, "bad JSON")
			return
		}
		t, err := u.Add(strings.TrimSpace(in.Name), in.Admin, in.Control, in.Groups)
		done(w, r, "add "+in.Name, err, map[string]string{"invite": t})
	}))
	m.HandleFunc("PUT /api/v1/users/{name}", admin(func(w http.ResponseWriter, r *http.Request) {
		var in userIn
		if !decode(r, &in) {
			bad(w, 400, "bad JSON")
			return
		}
		done(w, r, "update "+r.PathValue("name"), u.Update(r.PathValue("name"), in.Admin, in.Control, in.Groups), nil)
	}))
	m.HandleFunc("POST /api/v1/users/{name}/invite", admin(func(w http.ResponseWriter, r *http.Request) {
		t, err := u.Reinvite(r.PathValue("name"))
		done(w, r, "invite "+r.PathValue("name"), err, map[string]string{"invite": t})
	}))
	m.HandleFunc("POST /api/v1/users/{name}/logout", admin(func(w http.ResponseWriter, r *http.Request) {
		done(w, r, "logout "+r.PathValue("name"), u.LogoutAll(r.PathValue("name")), nil)
	}))
	m.HandleFunc("DELETE /api/v1/users/{name}", admin(func(w http.ResponseWriter, r *http.Request) {
		done(w, r, "delete "+r.PathValue("name"), u.Delete(r.PathValue("name")), nil)
	}))
	return m
}
