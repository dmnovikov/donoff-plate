package panel

// Users of the panel: an admin sees every device and gives the users groups of devices.
// A user is made by the admin and gets a one-time invite link where they set the password
// themselves (the same link resets a forgotten password). Passwords are kept as PBKDF2-SHA256;
// a session is a signed cookie (HMAC with the secret of the users file), so it survives a restart
// of the panel; "log out everywhere" bumps the user's generation.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdfIter     = 210000
	sessionTTL    = 30 * 24 * time.Hour
	inviteTTL     = 7 * 24 * time.Hour
	minPassword   = 8
	sessionCookie = "donoff_session"
)

type User struct {
	Name        string    `json:"name"`
	Admin       bool      `json:"admin"`
	Control     bool      `json:"control"` // may send commands to the devices of their groups
	Groups      []string  `json:"groups"`
	Hash        string    `json:"hash,omitempty"`   // "pbkdf2-sha256$<iter>$<salt hex>$<key hex>"
	Gen         int       `json:"gen"`              // in every session cookie; +1 = all sessions out
	Invite      string    `json:"invite,omitempty"` // sha256 of the invite token
	InviteUntil time.Time `json:"invite_until,omitzero"`
}

// UserView is a user for the admin page (no secrets).
type UserView struct {
	Name        string    `json:"name"`
	Admin       bool      `json:"admin"`
	Control     bool      `json:"control"`
	Groups      []string  `json:"groups"`
	HasPassword bool      `json:"has_password"`
	InviteUntil time.Time `json:"invite_until,omitzero"`
}

type Users struct {
	mu     sync.Mutex
	path   string
	Secret string  `json:"secret"`
	List   []*User `json:"users"`

	failMu sync.Mutex
	fails  []time.Time
}

var userRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{2,32}$`)

// LoadUsers reads the users file. On the first start (no file) it makes the user "admin" and
// gives its invite token, to be opened as /invite?t=<token>.
func LoadUsers(path string) (u *Users, firstInvite string, err error) {
	u = &Users{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		u.Secret = randHex(32)
		a := &User{Name: "admin", Admin: true, Control: true, Groups: []string{}}
		u.List = []*User{a}
		firstInvite = u.newInvite(a)
		return u, firstInvite, u.save()
	}
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(b, u); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if len(u.Secret) < 32 {
		return nil, "", fmt.Errorf("%s: no secret", path)
	}
	return u, "", nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func sha(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }

func (u *Users) save() error {
	if u.path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(u, "", "  ")
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.path)
}

func (u *Users) find(name string) *User {
	for _, x := range u.List {
		if x.Name == name {
			return x
		}
	}
	return nil
}

func (u *Users) newInvite(x *User) string {
	t := randHex(24)
	x.Invite, x.InviteUntil = sha(t), time.Now().Add(inviteTTL).UTC()
	return t
}

func hashPassword(p string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	k, err := pbkdf2.Key(sha256.New, p, salt, pbkdfIter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%x$%x", pbkdfIter, salt, k), nil
}

func checkPassword(hash, p string) bool {
	f := strings.Split(hash, "$")
	if len(f) != 4 || f[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(f[1])
	salt, err2 := hex.DecodeString(f[2])
	want, err3 := hex.DecodeString(f[3])
	if err != nil || err2 != nil || err3 != nil || iter < 1000 {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, p, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(k, want) == 1
}

func (u *Users) view(x *User) UserView {
	g := append([]string{}, x.Groups...)
	sort.Strings(g)
	v := UserView{Name: x.Name, Admin: x.Admin, Control: x.Control, Groups: g, HasPassword: x.Hash != ""}
	if x.Invite != "" && time.Now().Before(x.InviteUntil) {
		v.InviteUntil = x.InviteUntil
	}
	return v
}

func (u *Users) Views() []UserView {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := []UserView{}
	for _, x := range u.List {
		out = append(out, u.view(x))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (u *Users) admins() int {
	n := 0
	for _, x := range u.List {
		if x.Admin {
			n++
		}
	}
	return n
}

func cleanGroups(g []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range g {
		if s = strings.TrimSpace(s); s != "" && !seen[s] && len([]rune(s)) <= 40 {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Add makes a user and gives the invite token.
func (u *Users) Add(name string, admin, control bool, groups []string) (string, error) {
	if !userRe.MatchString(name) {
		return "", errors.New("a name is 2..32 of A-Z a-z 0-9 _ . @ -")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.find(name) != nil {
		return "", errors.New("this user exists")
	}
	x := &User{Name: name, Admin: admin, Control: control || admin, Groups: cleanGroups(groups)}
	t := u.newInvite(x)
	u.List = append(u.List, x)
	return t, u.save()
}

func (u *Users) Update(name string, admin, control bool, groups []string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(name)
	if x == nil {
		return errors.New("no such user")
	}
	if x.Admin && !admin && u.admins() == 1 {
		return errors.New("the last admin stays an admin")
	}
	x.Admin, x.Control, x.Groups = admin, control || admin, cleanGroups(groups)
	return u.save()
}

func (u *Users) Delete(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(name)
	if x == nil {
		return errors.New("no such user")
	}
	if x.Admin && u.admins() == 1 {
		return errors.New("the last admin can't be deleted")
	}
	for i, y := range u.List {
		if y == x {
			u.List = append(u.List[:i], u.List[i+1:]...)
			break
		}
	}
	return u.save()
}

// Reinvite gives a new invite token (to set or reset the password); the old password still works
// until the new one is set.
func (u *Users) Reinvite(name string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(name)
	if x == nil {
		return "", errors.New("no such user")
	}
	t := u.newInvite(x)
	return t, u.save()
}

// LogoutAll ends every session of the user.
func (u *Users) LogoutAll(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(name)
	if x == nil {
		return errors.New("no such user")
	}
	x.Gen++
	return u.save()
}

// InviteUser: the name of the user of a valid invite token ("" = none).
func (u *Users) InviteUser(token string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	h := sha(token)
	for _, x := range u.List {
		if x.Invite != "" && subtle.ConstantTimeCompare([]byte(x.Invite), []byte(h)) == 1 && time.Now().Before(x.InviteUntil) {
			return x.Name
		}
	}
	return ""
}

// AcceptInvite sets the password by the invite token; the token is spent, the old sessions end.
func (u *Users) AcceptInvite(token, password string) (string, error) {
	if len([]rune(password)) < minPassword {
		return "", fmt.Errorf("the password is at least %d characters", minPassword)
	}
	name := u.InviteUser(token)
	if name == "" {
		return "", errors.New("the link is not valid any more: ask the admin for a new one")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return "", err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(name)
	if x == nil {
		return "", errors.New("no such user")
	}
	x.Hash, x.Invite, x.InviteUntil = hash, "", time.Time{}
	x.Gen++
	return name, u.save()
}

func (u *Users) limited() bool {
	u.failMu.Lock()
	defer u.failMu.Unlock()
	cut := time.Now().Add(-time.Minute)
	for len(u.fails) > 0 && u.fails[0].Before(cut) {
		u.fails = u.fails[1:]
	}
	return len(u.fails) >= 10
}

func (u *Users) fail() {
	u.failMu.Lock()
	u.fails = append(u.fails, time.Now())
	u.failMu.Unlock()
}

// Login checks the password (at most 10 wrong logins a minute for all together).
func (u *Users) Login(name, password string) error {
	if u.limited() {
		return errors.New("too many wrong logins, wait a minute")
	}
	u.mu.Lock()
	x := u.find(name)
	hash := ""
	if x != nil {
		hash = x.Hash
	}
	u.mu.Unlock()
	if hash == "" || !checkPassword(hash, password) {
		u.fail()
		return errors.New("wrong name or password")
	}
	return nil
}

func (u *Users) sign(s string) string {
	m := hmac.New(sha256.New, []byte(u.Secret))
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

// Session is the cookie value for the user.
func (u *Users) Session(name string) string {
	u.mu.Lock()
	gen := 0
	if x := u.find(name); x != nil {
		gen = x.Gen
	}
	u.mu.Unlock()
	body := base64.RawURLEncoding.EncodeToString([]byte(name)) + "." + strconv.Itoa(gen) + "." + strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	return body + "." + u.sign(body)
}

// Who is the one behind a request: the user of the session, or the admin of the trusted networks.
type Who struct {
	Name    string          `json:"name"`
	Admin   bool            `json:"admin"`
	Control bool            `json:"control"`
	Groups  map[string]bool `json:"-"`
	Session bool            `json:"session"` // logged in (false: a trusted network)
}

func (w *Who) CanSee(group string) bool { return w.Admin || (group != "" && w.Groups[group]) }

// FromRequest checks the session cookie (nil = not logged in).
func (u *Users) FromRequest(r *http.Request) *Who {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	f := strings.Split(c.Value, ".")
	if len(f) != 4 || !hmac.Equal([]byte(u.sign(f[0]+"."+f[1]+"."+f[2])), []byte(f[3])) {
		return nil
	}
	nb, err := base64.RawURLEncoding.DecodeString(f[0])
	gen, err2 := strconv.Atoi(f[1])
	exp, err3 := strconv.ParseInt(f[2], 10, 64)
	if err != nil || err2 != nil || err3 != nil || time.Now().Unix() > exp {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.find(string(nb))
	if x == nil || x.Gen != gen || x.Hash == "" {
		return nil
	}
	w := &Who{Name: x.Name, Admin: x.Admin, Control: x.Control || x.Admin, Groups: map[string]bool{}, Session: true}
	for _, g := range x.Groups {
		w.Groups[g] = true
	}
	return w
}

func setSession(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}
