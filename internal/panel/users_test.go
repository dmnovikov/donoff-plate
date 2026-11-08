package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	h, err := hashPassword("secret-pass")
	if err != nil || !checkPassword(h, "secret-pass") || checkPassword(h, "secret-pasS") || checkPassword("junk", "x") {
		t.Fatalf("%q %v", h, err)
	}
}

func TestUsersFile(t *testing.T) {
	f := t.TempDir() + "/users.json"
	u, inv, err := LoadUsers(f)
	if err != nil || inv == "" || len(u.List) != 1 || !u.List[0].Admin {
		t.Fatalf("first start: %v %q %+v", err, inv, u.List)
	}
	if err := u.Login("admin", "whatever1"); err == nil {
		t.Fatal("no password yet: no login")
	}
	if _, err := u.AcceptInvite(inv, "short"); err == nil {
		t.Fatal("short password")
	}
	if n, err := u.AcceptInvite(inv, "admin-pass-1"); err != nil || n != "admin" {
		t.Fatal(n, err)
	}
	if _, err := u.AcceptInvite(inv, "admin-pass-2"); err == nil {
		t.Fatal("an invite works once")
	}
	if err := u.Delete("admin"); err == nil {
		t.Fatal("the last admin can't go")
	}
	if err := u.Update("admin", false, true, nil); err == nil {
		t.Fatal("the last admin stays")
	}
	u2, inv2, err := LoadUsers(f) // the file is kept
	if err != nil || inv2 != "" || u2.Login("admin", "admin-pass-1") != nil {
		t.Fatal("reload:", err, inv2)
	}
}

// the users' listener: who sees which device and may do what
func TestUsersHTTP(t *testing.T) {
	h := New(Creds{User: "donoff"})
	for _, d := range []string{"dmzk10", "dmzk13", "t14"} {
		h.onMessage("/donoff/", "/donoff/"+d+"/out/relays/r1", "0")
	}
	h.SetGroup("dmzk10", "Доможирка")
	h.SetGroup("dmzk13", "Ташкент")
	u, inv, _ := LoadUsers(t.TempDir() + "/u.json")
	u.AcceptInvite(inv, "admin-pass-1")
	ti, _ := u.Add("anna", false, false, []string{"Доможирка"})
	u.AcceptInvite(ti, "anna-pass-1")
	srv := httptest.NewServer(h.UsersHandler(u))
	defer srv.Close()
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	login := func(name, pass string) *http.Client {
		c := &http.Client{CheckRedirect: noRedirect}
		r, err := c.PostForm(srv.URL+"/login", url.Values{"name": {name}, "password": {pass}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		for _, ck := range r.Cookies() {
			if ck.Name == sessionCookie && ck.Value != "" {
				c.Jar = jarWith(srv.URL, ck)
				return c
			}
		}
		return nil
	}
	get := func(c *http.Client, path string) (int, map[string]any) {
		r, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	send := func(c *http.Client, method, path, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	if login("anna", "wrong-pass") != nil {
		t.Fatal("wrong password logged in")
	}
	if code, _ := get(&http.Client{CheckRedirect: noRedirect}, "/api/v1/state"); code != 401 {
		t.Fatalf("no session: %d", code)
	}
	anna := login("anna", "anna-pass-1")
	if anna == nil {
		t.Fatal("anna: no session")
	}
	code, st := get(anna, "/api/v1/state")
	devs := st["devices"].([]any)
	if code != 200 || len(devs) != 1 || devs[0].(map[string]any)["id"] != "dmzk10" {
		t.Fatalf("anna sees %v", devs)
	}
	if c, _ := get(anna, "/api/v1/history?dev=dmzk13&range=1h"); c != 403 {
		t.Fatalf("history of another group: %d", c)
	}
	if c := send(anna, "POST", "/api/v1/cmd", `{"dev":"dmzk13","cmd":"sh time"}`); c != 403 {
		t.Fatalf("cmd to another group: %d", c)
	}
	if c := send(anna, "POST", "/api/v1/cmd", `{"dev":"dmzk10","cmd":"r11"}`); c != 403 {
		t.Fatalf("a viewer switched a relay: %d", c)
	}
	if c := send(anna, "PUT", "/api/v1/group", `{"dev":"t14","group":"Доможирка"}`); c != 403 {
		t.Fatalf("a user set a group: %d", c)
	}
	if c, _ := get(anna, "/api/v1/users"); c != 403 {
		t.Fatalf("a user read the users: %d", c)
	}

	admin := login("admin", "admin-pass-1")
	if _, st := get(admin, "/api/v1/state"); len(st["devices"].([]any)) != 3 {
		t.Fatal("the admin sees all")
	}
	if c := send(admin, "PUT", "/api/v1/users/anna", `{"admin":false,"control":true,"groups":["Доможирка","Ташкент"]}`); c != 200 {
		t.Fatalf("update: %d", c)
	}
	if _, st := get(anna, "/api/v1/state"); len(st["devices"].([]any)) != 2 {
		t.Fatal("anna got Ташкент")
	}
	if c := send(admin, "POST", "/api/v1/users/anna/logout", ``); c != 200 {
		t.Fatalf("logout: %d", c)
	}
	if c, _ := get(anna, "/api/v1/state"); c != 401 {
		t.Fatalf("after logout everywhere: %d", c)
	}
}

type oneJar struct{ c *http.Cookie }

func (j *oneJar) SetCookies(*url.URL, []*http.Cookie) {}
func (j *oneJar) Cookies(*url.URL) []*http.Cookie     { return []*http.Cookie{j.c} }
func jarWith(_ string, c *http.Cookie) http.CookieJar { return &oneJar{c} }
