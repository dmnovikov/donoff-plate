package panel

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenGate lets in only the requests with the token (a cookie after the login form, or
// "Authorization: Bearer <token>"), whatever the address: for a listener behind a tunnel, where
// every connection comes from loopback. At most 10 wrong tokens a minute (for all together).
func TokenGate(token string, next http.Handler) http.Handler {
	var mu sync.Mutex
	var fails []time.Time
	ok := func(s string) bool { return s != "" && subtle.ConstantTimeCompare([]byte(s), []byte(token)) == 1 }
	limited := func() bool {
		mu.Lock()
		defer mu.Unlock()
		cut := time.Now().Add(-time.Minute)
		for len(fails) > 0 && fails[0].Before(cut) {
			fails = fails[1:]
		}
		return len(fails) >= 10
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/login" {
			if limited() {
				http.Error(w, "too many wrong tokens, wait a minute", http.StatusTooManyRequests)
				return
			}
			if !ok(r.PostFormValue("token")) {
				mu.Lock()
				fails = append(fails, time.Now())
				mu.Unlock()
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write(loginPage("неверный токен"))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "donoff_token", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 86400})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, _ := r.Cookie("donoff_token")
		auth := r.Header.Get("Authorization")
		if (c != nil && ok(c.Value)) || (len(auth) > 7 && auth[:7] == "Bearer " && ok(auth[7:])) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == "GET" && !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(loginPage(""))
			return
		}
		reply(w, 401, map[string]string{"error": "token required"})
	})
}

func loginPage(msg string) []byte {
	return []byte(`<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>donoff</title>
<style>body{margin:0;font:15px system-ui,sans-serif;background:#f4f5f8;color:#1d2330;display:grid;place-items:center;min-height:100vh}
@media (prefers-color-scheme:dark){body{background:#12151b;color:#e3e7ee}}
form{display:grid;gap:10px;width:min(320px,calc(100vw - 32px))}input,button{font:inherit;padding:8px 10px;border-radius:7px;border:1px solid #8b94a7}
button{background:#2563eb;color:#fff;border-color:#2563eb;cursor:pointer}.e{color:#c2410c;min-height:1em}</style></head>
<body><form method="post" action="/login"><b>donoff — вход</b><input name="token" type="password" placeholder="токен" autofocus autocomplete="current-password"><button>Войти</button><div class="e">` + msg + `</div></form></body></html>`)
}
