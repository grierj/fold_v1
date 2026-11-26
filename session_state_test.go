package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"fold/internal/testdb"
	"fold/models"
)

// sessionCurrent is the one decision every recheck makes: a row that can sign
// in and carries both of the session's epochs.
func TestSessionCurrentPredicate(t *testing.T) {
	hash := "x"
	live := func() *models.User {
		return &models.User{ID: 7, Status: models.UserActive, PasswordHash: &hash, SessionVersion: 2, LogoutVersion: 3}
	}
	session := &models.User{ID: 7, SessionVersion: 2, LogoutVersion: 3}
	cases := map[string]func(u *models.User){
		"current":        func(u *models.User) {},
		"session epoch":  func(u *models.User) { u.SessionVersion++ },
		"logout epoch":   func(u *models.User) { u.LogoutVersion++ },
		"disabled":       func(u *models.User) { u.Status = models.UserDisabled },
		"no password":    func(u *models.User) { u.PasswordHash = nil },
		"soft deleted":   func(u *models.User) { u.DeletedAt = gorm.DeletedAt{Valid: true} },
		"another person": func(u *models.User) { u.ID = 8 },
	}
	for name, mutate := range cases {
		u := live()
		mutate(u)
		if got, want := sessionCurrent(u, session), name == "current"; got != want {
			t.Errorf("%s: sessionCurrent = %v, want %v", name, got, want)
		}
	}
	if sessionCurrent(nil, session) || sessionCurrent(live(), nil) {
		t.Error("a nil row or session was current")
	}
}

// lockActorIn answers errSecuritySession for every way a session goes stale,
// whichever lock mode the site uses, and lockAdminIn also needs the role.
func TestLockActorInRefusesEveryStaleSession(t *testing.T) {
	app := testApp(t)
	member := newTestUser(t, app, "actor-member", "password1234", models.RoleMember)
	admin := newTestUser(t, app, "actor-admin", "password1234", models.RoleAdmin)
	for _, mode := range []actorLock{actorRead, actorShare, actorNoKeyUpdate} {
		for name, session := range map[string]*models.User{
			"current":       {ID: member.ID, SessionVersion: member.SessionVersion, LogoutVersion: member.LogoutVersion},
			"session epoch": {ID: member.ID, SessionVersion: member.SessionVersion + 1, LogoutVersion: member.LogoutVersion},
			"logout epoch":  {ID: member.ID, SessionVersion: member.SessionVersion, LogoutVersion: member.LogoutVersion + 1},
			"unknown":       {ID: 999999},
		} {
			err := app.DB.Transaction(func(tx *gorm.DB) error {
				got, err := lockActorIn(tx, session, mode)
				if err == nil && got.ID != member.ID {
					t.Errorf("mode %d %s: returned user %d", mode, name, got.ID)
				}
				return err
			})
			if stale := name != "current"; stale != errors.Is(err, errSecuritySession) || (!stale && err != nil) {
				t.Errorf("mode %d %s: err = %v", mode, name, err)
			}
		}
		err := app.DB.Transaction(func(tx *gorm.DB) error {
			_, err := lockAdminIn(tx, member, mode)
			return err
		})
		if !errors.Is(err, errSecuritySession) {
			t.Errorf("mode %d: a current non-admin was accepted: %v", mode, err)
		}
		err = app.DB.Transaction(func(tx *gorm.DB) error {
			_, err := lockAdminIn(tx, admin, mode)
			return err
		})
		if err != nil {
			t.Errorf("mode %d: a current admin was refused: %v", mode, err)
		}
	}
}

// endAllSessionsIn bumps the credential epoch, applies the caller's columns in
// the same update and revokes every outstanding capability.
func TestEndAllSessionsIn(t *testing.T) {
	app := testApp(t)
	u := newTestUser(t, app, "end-all", "password1234", models.RoleMember)
	token := models.AccountToken{Kind: models.TokenPasswordReset, UserID: u.ID, TokenHash: hashToken("end-all"), Payload: `{}`, ExpiresAt: u.CreatedAt.AddDate(1, 0, 0)}
	if err := app.DB.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB.Transaction(func(tx *gorm.DB) error {
		return endAllSessionsIn(tx, u, map[string]any{"name": "Renamed"})
	}); err != nil {
		t.Fatal(err)
	}
	var after models.User
	if err := app.DB.First(&after, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.SessionVersion != u.SessionVersion+1 || after.Name != "Renamed" || after.LogoutVersion != u.LogoutVersion {
		t.Errorf("epochs/columns after: session %d (was %d), logout %d (was %d), name %q", after.SessionVersion, u.SessionVersion, after.LogoutVersion, u.LogoutVersion, after.Name)
	}
	var left int64
	app.DB.Model(&models.AccountToken{}).Where("user_id = ? AND used_at IS NULL", u.ID).Count(&left)
	if left != 0 {
		t.Errorf("%d capabilities survived", left)
	}
}

// credentialOf reads the browser credential a client's jar holds: T from the
// signed session cookie, decoded by the app's own store, and the marker cookie.
func credentialOf(t *testing.T, c *apiClient) (token, marker string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	if ck := c.jar[sessionName]; ck != nil && ck.MaxAge >= 0 {
		req.AddCookie(ck)
	}
	s, err := c.app.Store.New(req, sessionName)
	if err != nil {
		t.Fatalf("decode session: %v", err)
	}
	token, _ = s.Values[csrfSessionKey].(string)
	if ck := c.jar[c.app.credentialCookieName()]; ck != nil && ck.MaxAge >= 0 {
		marker = ck.Value
	}
	return token, marker
}

// pinnedAs is a copy of c whose requests are participating ones (M8.1),
// sharing c's jar. Its T must be the one its cookie holds.
func pinnedAs(t *testing.T, c *apiClient) *apiClient {
	t.Helper()
	token, marker := credentialOf(t, c)
	if token == "" || marker == "" || token != c.token {
		t.Fatalf("pinnedAs: the client holds no usable credential (T %t, M %t, matches %t)", token != "", marker != "", token == c.token)
	}
	p := *c
	p.pinned = true
	return &p
}

// wantAck checks w acknowledges c's credential for viewer (nil: anonymous),
// once, and set no cookie.
func wantAck(t *testing.T, what string, w *httptest.ResponseRecorder, c *apiClient, viewer *models.User) {
	t.Helper()
	_, marker := credentialOf(t, c)
	who := "anon"
	if viewer != nil {
		who = itoa(viewer.ID)
	}
	if got := w.Header().Values(sessionCheckedHeader); len(got) != 1 || got[0] != "v2 "+marker+" "+who {
		t.Fatalf("%s: acknowledgment %q, want %q (%d %s)", what, got, "v2 "+marker+" "+who, w.Code, w.Body.String())
	}
	if cookies := w.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("%s: a participating answer set cookies %v", what, cookies)
	}
}

// wantNoAck checks w carries no acknowledgment.
func wantNoAck(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := w.Header().Values(sessionCheckedHeader); len(got) != 0 {
		t.Fatalf("%s: acknowledged %q (%d %s)", what, got, w.Code, w.Body.String())
	}
}

// TestDecidedRefusalIsAnActionable4xx: only a declared 4xx answer other than
// the 401 counts as a refusal a reader decided; a 5xx, the stale session, a
// refusal whose snapshot rollback was not confirmed and an unplanned error
// never do.
func TestDecidedRefusalIsAnActionable4xx(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errEventNotFound, true},
		{errCannotChangeEvent, true},
		{errMembersOnly, true},
		{fmt.Errorf("wrapped: %w", errDateChanged), true},
		{dateNeedsConsent{}, true},
		{errSecuritySession, false},
		{fmt.Errorf("wrapped: %w", errSecuritySession), false},
		{errCSRFSession, false},
		{&snapshotRollbackError{cause: errEventNotFound, rollback: errors.New("conn closed")}, false},
		{errors.New("unplanned"), false},
	} {
		if got := decidedRefusal(tc.err); got != tc.want {
			t.Errorf("decidedRefusal(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

// wantFreshPair checks one response replaced the credential: exactly one
// Set-Cookie for the session and for the marker, the marker readable by
// script with the session's path, SameSite, Secure and lifetime, and equal to
// the HMAC of the new T, which differs from the old one.
func wantFreshPair(t *testing.T, c *apiClient, w *httptest.ResponseRecorder, oldToken string) string {
	t.Helper()
	var sess, mark []*http.Cookie
	for _, ck := range w.Result().Cookies() {
		switch ck.Name {
		case sessionName:
			sess = append(sess, ck)
		case c.app.credentialCookieName():
			mark = append(mark, ck)
		}
	}
	if len(sess) != 1 || len(mark) != 1 {
		t.Fatalf("Set-Cookie: %d session, %d marker; want one each (%v)", len(sess), len(mark), w.Header().Values("Set-Cookie"))
	}
	m := mark[0]
	if m.HttpOnly || m.Path != "/" || m.Domain != "" || m.SameSite != sess[0].SameSite || m.Secure != sess[0].Secure || m.MaxAge != sess[0].MaxAge || m.MaxAge <= 0 {
		t.Fatalf("marker cookie %+v does not match session cookie %+v", m, sess[0])
	}
	token, marker := credentialOf(t, c)
	want, err := c.app.credentialMarker(token)
	if err != nil || token == "" || marker != want {
		t.Fatalf("marker %q is not the HMAC of T (%v)", marker, err)
	}
	if token == oldToken {
		t.Fatal("the credential was not replaced")
	}
	return token
}

// Every credential replacement mints a fresh T and its marker in one response:
// sign-in (fresh and same account again), a password change, a verified email
// in this browser, a reset, an account setup, an accepted invitation and a
// pending teen's sign-in that clears the login.
func TestCredentialReplacementsMintAFreshPair(t *testing.T) {
	for _, path := range []string{"login", "same-user login", "password", "email", "reset", "setup", "invitation", "pending clear"} {
		t.Run(path, func(t *testing.T) {
			app := testApp(t)
			user := newTestUser(t, app, "alice", "password1234", models.RoleMember)
			c := newAPIClient(t, app)
			c.fetchCSRF()
			var w *httptest.ResponseRecorder
			signedIn := true
			if path != "login" && path != "reset" && path != "setup" && path != "invitation" && path != "pending clear" {
				c = loginAPI(t, app, "alice", "password1234")
			}
			old, _ := credentialOf(t, c)
			switch path {
			case "login", "same-user login":
				w = c.do("POST", "/api/v1/auth/login", map[string]string{"email": user.Email, "password": "password1234"}, true)
			case "password":
				w = c.do("POST", "/api/v1/me/password", map[string]string{"current": "password1234", "password": "replacement1234"}, true)
			case "email":
				raw, _, err := issueAccountToken(app.DB, models.TokenEmailVerify, user.ID, map[string]any{"new_email": "alice.new@example.com"}, tokenTTL)
				if err != nil {
					t.Fatal(err)
				}
				w = c.do("POST", "/api/v1/me/email/verify", map[string]string{"token": raw}, true)
			case "reset", "setup":
				kind := map[string]string{"reset": models.TokenPasswordReset, "setup": models.TokenAccountSetup}[path]
				raw, _, err := issueAccountToken(app.DB, kind, user.ID, nil, tokenTTL)
				if err != nil {
					t.Fatal(err)
				}
				route := map[string]string{"reset": "/api/v1/auth/password-reset/confirm", "setup": "/api/v1/account-requests/confirm"}[path]
				w = c.do("POST", route, map[string]string{"token": raw, "password": "replacement1234"}, true)
			case "invitation":
				leader := loginAPI(t, app, "alice", "password1234")
				g := newTestGroup(t, app, user, "Credential study", models.GroupActive)
				token := invitedIntoGroup(t, app, leader, "invited@example.com", g.ID)
				w = c.do("POST", "/api/v1/invitations/accept", acceptBody(token), true)
			case "pending clear":
				c = loginAPI(t, app, "alice", "password1234")
				old, _ = credentialOf(t, c)
				app.DB.Model(user).Updates(map[string]any{"account_type": models.AccountTeen, "status": models.UserPendingConsent})
				w = c.do("POST", "/api/v1/auth/login", map[string]string{"email": user.Email, "password": "password1234"}, true)
				signedIn = false
			}
			if w.Code != http.StatusOK && !(path == "pending clear" && w.Code == http.StatusForbidden) {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
			}
			wantFreshPair(t, c, w, old)
			me := c.do("GET", "/api/v1/auth/me", nil, false)
			if (me.Code == http.StatusOK) != signedIn {
				t.Fatalf("after %s, /auth/me = %d", path, me.Code)
			}
			// The new T is the CSRF token: an unsafe request with it passes.
			if w := c.do("POST", "/api/v1/auth/logout", nil, true); w.Code != http.StatusOK {
				t.Fatalf("logout with the new token: %d", w.Code)
			}
		})
	}
}

// A verified email changes this browser's credential and credential epoch
// only: a cookie already signed out elsewhere stays signed out (its logout
// epoch is kept), and another account's or a signed-out browser's cookie gets
// no Set-Cookie at all.
func TestVerifiedEmailReplacesOnlyItsOwnCurrentCredential(t *testing.T) {
	verify := func(t *testing.T, app *App, user *models.User, c *apiClient) *httptest.ResponseRecorder {
		t.Helper()
		raw, _, err := issueAccountToken(app.DB, models.TokenEmailVerify, user.ID, map[string]any{"new_email": "moved-" + itoa(user.ID) + "@example.com"}, tokenTTL)
		if err != nil {
			t.Fatal(err)
		}
		w := c.do("POST", "/api/v1/me/email/verify", map[string]string{"token": raw}, true)
		if w.Code != http.StatusOK {
			t.Fatalf("verify: %d %s", w.Code, w.Body.String())
		}
		return w
	}
	t.Run("signed out elsewhere", func(t *testing.T) {
		app := testApp(t)
		alice := newTestUser(t, app, "alice", "password1234", models.RoleMember)
		stale := loginAPI(t, app, "alice", "password1234")
		other := loginAPI(t, app, "alice", "password1234")
		apiDo(t, other, "POST", "/api/v1/auth/logout", nil, http.StatusOK)
		old, _ := credentialOf(t, stale)
		wantFreshPair(t, stale, verify(t, app, alice, stale), old)
		if w := stale.do("GET", "/api/v1/auth/me", nil, false); w.Code != http.StatusUnauthorized {
			t.Fatalf("a cookie signed out everywhere was signed back in: %d", w.Code)
		}
	})
	for _, who := range []string{"another account", "signed out"} {
		t.Run(who, func(t *testing.T) {
			app := testApp(t)
			alice := newTestUser(t, app, "alice", "password1234", models.RoleMember)
			newTestUser(t, app, "bob", "password1234", models.RoleMember)
			c := newAPIClient(t, app)
			c.fetchCSRF()
			if who == "another account" {
				c = loginAPI(t, app, "bob", "password1234")
			}
			before, marker := credentialOf(t, c)
			w := verify(t, app, alice, c)
			if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
				t.Fatalf("%s got Set-Cookie %v", who, got)
			}
			if after, m := credentialOf(t, c); after != before || m != marker {
				t.Fatalf("%s: credential changed", who)
			}
			if w := c.do("GET", "/api/v1/auth/me", nil, false); (w.Code == http.StatusOK) != (who == "another account") {
				t.Fatalf("%s: /auth/me = %d", who, w.Code)
			}
		})
	}
}

// Logout expires the session and its marker together, under either name.
func TestLogoutExpiresTheCredentialPair(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "alice", "password1234", models.RoleMember)
	c := loginAPI(t, app, "alice", "password1234")
	c.jar[app.otherCredentialCookieName()] = &http.Cookie{Name: app.otherCredentialCookieName(), Value: "left-from-a-switch"}
	w := c.do("POST", "/api/v1/auth/logout", nil, true)
	got := map[string]*http.Cookie{}
	for _, ck := range w.Result().Cookies() {
		if _, dup := got[ck.Name]; dup {
			t.Fatalf("two Set-Cookie for %s", ck.Name)
		}
		got[ck.Name] = ck
	}
	for _, name := range []string{sessionName, app.credentialCookieName(), app.otherCredentialCookieName()} {
		if ck := got[name]; ck == nil || ck.MaxAge >= 0 {
			t.Fatalf("%s not expired: %+v", name, ck)
		}
	}
}

// A failed mint, or a session that cannot be encoded, writes neither cookie:
// the browser keeps the credential it had, and a database change that already
// committed (a password change) leaves that cookie stale, never signed in as
// a replayed change.
func TestCredentialFailuresWriteNoCookie(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "alice", "password1234", models.RoleMember)
	c := loginAPI(t, app, "alice", "password1234")
	app.hooks.credentialEntropy = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	noCookies := func(what string, w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatalf("%s: %d, Set-Cookie %v", what, w.Code, w.Header().Values("Set-Cookie"))
		}
	}
	noCookies("login", c.do("POST", "/api/v1/auth/login", map[string]string{"email": testEmail("alice"), "password": "password1234"}, true), http.StatusInternalServerError)
	noCookies("password", c.do("POST", "/api/v1/me/password", map[string]string{"current": "password1234", "password": "replacement1234"}, true), http.StatusInternalServerError)
	first := newAPIClient(t, app)
	w := httptest.NewRecorder()
	first.router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/auth/bootstrap", nil))
	noCookies("first visit", w, http.StatusInternalServerError)
	app.hooks.credentialEntropy = nil
	var boot sessionBootstrapJSON
	decodeJSON(t, c.do("GET", "/api/v1/auth/bootstrap", nil, false), &boot)
	if boot.User != nil {
		t.Fatal("the committed password change left the old cookie signed in")
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	s, _ := app.Store.Get(ctx.Request, sessionName)
	s.Values[csrfSessionKey] = "t"
	s.Values["unencodable"] = func() {}
	if err := app.writeSession(ctx, s); err == nil || len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("an unencodable session: %v, Set-Cookie %v", err, rec.Header().Values("Set-Cookie"))
	}
}

// browserJar drives the real router through net/http/cookiejar, a real jar:
// it keeps host-only cookies per host (the plain and the TLS test server share
// 127.0.0.1), sends a Secure cookie only over HTTPS and drops an expired one.
type browserJar struct {
	t      *testing.T
	jar    *cookiejar.Jar
	client *http.Client
	plain  *httptest.Server
	tls    *httptest.Server
}

func newBrowserJar(t *testing.T, handler http.Handler) *browserJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &browserJar{t: t, jar: jar, plain: httptest.NewServer(handler), tls: httptest.NewTLSServer(handler)}
	t.Cleanup(b.plain.Close)
	t.Cleanup(b.tls.Close)
	b.client = &http.Client{Jar: jar, Transport: b.tls.Client().Transport}
	return b
}

func (b *browserJar) send(srv *httptest.Server, method, path string, body any, header map[string]string) (*http.Response, []byte) {
	b.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		b.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	return res, out
}

// markers is every marker cookie the jar would send to srv, by name.
func (b *browserJar) markers(srv *httptest.Server) map[string]string {
	u, _ := url.Parse(srv.URL)
	got := map[string]string{}
	for _, ck := range b.jar.Cookies(u) {
		if ck.Name == credentialCookieSecure || ck.Name == credentialCookiePlain {
			got[ck.Name] = ck.Value
		}
	}
	return got
}

func (b *browserJar) bootstrap(srv *httptest.Server) (*http.Response, sessionBootstrapJSON) {
	b.t.Helper()
	res, raw := b.send(srv, "GET", "/api/v1/auth/bootstrap", nil, nil)
	var body sessionBootstrapJSON
	if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &body) != nil {
		b.t.Fatalf("bootstrap: %d %s", res.StatusCode, raw)
	}
	return res, body
}

func (b *browserJar) csrf(srv *httptest.Server) string {
	b.t.Helper()
	res, raw := b.send(srv, "GET", "/api/v1/auth/csrf", nil, nil)
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &body) != nil || body.CSRFToken == "" {
		b.t.Fatalf("csrf: %d %s", res.StatusCode, raw)
	}
	return body.CSRFToken
}

func (b *browserJar) login(srv *httptest.Server, email string) *http.Response {
	b.t.Helper()
	res, raw := b.send(srv, "POST", "/api/v1/auth/login", map[string]string{"email": email, "password": "password1234"}, map[string]string{csrfHeader: b.csrf(srv)})
	if res.StatusCode != http.StatusOK {
		b.t.Fatalf("login: %d %s", res.StatusCode, raw)
	}
	return res
}

// wantOtherNameExpired: the response expires the other marker name the way a
// browser accepts for it (host-only, path /, Secure, which __Host- requires).
func wantOtherNameExpired(t *testing.T, res *http.Response, name string) {
	t.Helper()
	for _, ck := range res.Cookies() {
		if ck.Name == name {
			if ck.MaxAge >= 0 || ck.Value != "" || ck.Path != "/" || ck.Domain != "" || !ck.Secure || ck.HttpOnly {
				t.Fatalf("the expiry of %s is %+v", name, ck)
			}
			return
		}
	}
	t.Fatalf("no expiry of %s in %v", name, res.Header.Values("Set-Cookie"))
}

// A deployment that switches between plain HTTP and HTTPS on one host leaves
// the marker the old setting named in the browser beside the new one, and
// script that sees both trusts neither. The next bootstrap or credential write
// expires the old name, so the jar holds one marker, a pinned request works
// with it, and logout leaves neither. The reverse switch is shown over TLS, as
// a browser on localhost sends a Secure cookie to http://localhost; elsewhere a
// plain-HTTP page neither receives nor shows script the Secure cookie.
func TestMarkerCookieRollsOverBetweenHTTPAndHTTPS(t *testing.T) {
	app := testApp(t)
	alice := newTestUser(t, app, "alice", "password1234", models.RoleMember)
	r, reached := pinnedFixture(t, app)
	t.Cleanup(func() { app.Store.Options.Secure = false })
	one := func(b *browserJar, srv *httptest.Server, name, value string) {
		t.Helper()
		if got := b.markers(srv); len(got) != 1 || got[name] != value || value == "" {
			t.Fatalf("jar markers %v, want only %s=%s", got, name, value)
		}
	}

	t.Run("HTTP to HTTPS, repaired by bootstrap", func(t *testing.T) {
		b := newBrowserJar(t, r)
		app.Store.Options.Secure = false
		b.login(b.plain, alice.Email)
		if got := b.markers(b.plain); len(got) != 1 || got[credentialCookiePlain] == "" {
			t.Fatalf("after a plain sign-in the jar holds %v", got)
		}
		app.Store.Options.Secure = true
		res, body := b.bootstrap(b.tls)
		if body.User == nil || body.User.ID != alice.ID {
			t.Fatalf("bootstrap after the switch: %+v", body.User)
		}
		wantOtherNameExpired(t, res, credentialCookiePlain)
		one(b, b.tls, credentialCookieSecure, body.CredentialMarker)
		if got := res.Header.Get(sessionCheckedHeader); got != "v2 "+body.CredentialMarker+" "+itoa(alice.ID) {
			t.Fatalf("acknowledgment %q", got)
		}
		// A second bootstrap finds one good marker and writes nothing.
		if again, _ := b.bootstrap(b.tls); len(again.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("a repaired jar was written again: %v", again.Header.Values("Set-Cookie"))
		}
		before := *reached
		pinned, _ := b.send(b.tls, "GET", "/api/v1/test/pinned", nil, map[string]string{sessionCheckHeader: sessionCheckVersion, csrfHeader: body.CSRFToken})
		if pinned.StatusCode != http.StatusOK || *reached != before+1 || pinned.Header.Get(sessionCheckedHeader) != "v2 "+body.CredentialMarker+" "+itoa(alice.ID) {
			t.Fatalf("pinned after the repair: %d %q", pinned.StatusCode, pinned.Header.Get(sessionCheckedHeader))
		}
		out, _ := b.send(b.tls, "POST", "/api/v1/auth/logout", nil, map[string]string{csrfHeader: body.CSRFToken})
		if out.StatusCode != http.StatusOK || len(b.markers(b.tls)) != 0 {
			t.Fatalf("logout: %d, jar markers %v", out.StatusCode, b.markers(b.tls))
		}
	})

	t.Run("HTTP to HTTPS, repaired by the next sign-in", func(t *testing.T) {
		b := newBrowserJar(t, r)
		app.Store.Options.Secure = false
		b.login(b.plain, alice.Email)
		app.Store.Options.Secure = true
		b.csrf(b.tls)
		res := b.login(b.tls, alice.Email)
		wantOtherNameExpired(t, res, credentialCookiePlain)
		_, body := b.bootstrap(b.tls)
		one(b, b.tls, credentialCookieSecure, body.CredentialMarker)
	})

	t.Run("HTTPS to HTTP on localhost", func(t *testing.T) {
		b := newBrowserJar(t, r)
		app.Store.Options.Secure = true
		b.login(b.tls, alice.Email)
		if got := b.markers(b.tls); len(got) != 1 || got[credentialCookieSecure] == "" {
			t.Fatalf("after a Secure sign-in the jar holds %v", got)
		}
		app.Store.Options.Secure = false
		res, body := b.bootstrap(b.tls)
		wantOtherNameExpired(t, res, credentialCookieSecure)
		one(b, b.tls, credentialCookiePlain, body.CredentialMarker)
		out, _ := b.send(b.tls, "POST", "/api/v1/auth/logout", nil, map[string]string{csrfHeader: body.CSRFToken})
		if out.StatusCode != http.StatusOK || len(b.markers(b.tls)) != 0 {
			t.Fatalf("logout: %d, jar markers %v", out.StatusCode, b.markers(b.tls))
		}
	})
}

// markersAt is every marker cookie, by name and value, that the jar holds for
// a request to path on srv, longest path first as a browser orders them, so
// two cookies of one name both show.
func (b *browserJar) markersAt(srv *httptest.Server, path string) []string {
	u, _ := url.Parse(srv.URL + path)
	var got []string
	for _, ck := range b.jar.Cookies(u) {
		if ck.Name == credentialCookieSecure || ck.Name == credentialCookiePlain {
			got = append(got, ck.Name+"="+ck.Value)
		}
	}
	return got
}

// A marker copy of the same name at another path, left by something else on
// this host, is a different cookie: the browser sends it only to requests
// under its path, and neither a request nor script says which path a cookie
// has. A copy under /api reaches the server beside its own marker; the server
// cannot tell which is its own, so bootstrap sets its own at / again, and it
// cannot expire the copy, whose path it does not know. A copy under a page
// path (/groups) never reaches the server at all. The session check reads T
// from the signed cookie, so pinned requests still pass; script on a page
// under the copy's path sees two markers and trusts neither (core.test.ts),
// which fails closed until the copy expires or is cleared. Over HTTPS the
// copy has the plain name, since __Host- forbids a path but /, and expiring
// the plain name at / leaves it too.
func TestMarkerCopiesAtOtherPathsStayAndFailClosed(t *testing.T) {
	app := testApp(t)
	alice := newTestUser(t, app, "alice", "password1234", models.RoleMember)
	r, reached := pinnedFixture(t, app)
	t.Cleanup(func() { app.Store.Options.Secure = false })

	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "Secure"}[secure], func(t *testing.T) {
			app.Store.Options.Secure = secure
			b := newBrowserJar(t, r)
			srv, own := b.plain, credentialCookiePlain
			if secure {
				srv, own = b.tls, credentialCookieSecure
			}
			b.login(srv, alice.Email)
			u, _ := url.Parse(srv.URL)
			b.jar.SetCookies(u, []*http.Cookie{
				{Name: credentialCookiePlain, Value: "copy-api", Path: "/api/v1"},
				{Name: credentialCookiePlain, Value: "copy-groups", Path: "/groups"},
			})

			res, body := b.bootstrap(srv)
			mine := own + "=" + body.CredentialMarker
			if body.User == nil || body.User.ID != alice.ID || res.Header.Get(sessionCheckedHeader) != "v2 "+body.CredentialMarker+" "+itoa(alice.ID) {
				t.Fatalf("bootstrap beside the copies: %+v %q", body.User, res.Header.Get(sessionCheckedHeader))
			}
			set := false
			for _, ck := range res.Cookies() {
				if ck.Name == own && ck.Value == body.CredentialMarker && ck.Path == "/" {
					set = true
				}
				if ck.Name == credentialCookiePlain && ck.Path != "/" {
					t.Fatalf("the server named a copy's path: %+v", ck)
				}
			}
			if !set {
				t.Fatalf("bootstrap did not set its own marker beside a copy it saw: %v", res.Header.Values("Set-Cookie"))
			}
			for path, want := range map[string][]string{
				"/api/v1/auth/bootstrap": {"fold-credential=copy-api", mine},
				"/groups/7":              {"fold-credential=copy-groups", mine},
				"/":                      {mine},
			} {
				if got := b.markersAt(srv, path); !slices.Equal(got, want) {
					t.Fatalf("after bootstrap the jar sends %v to %s, want %v", got, path, want)
				}
			}

			before := *reached
			pinned, _ := b.send(srv, "GET", "/api/v1/test/pinned", nil, map[string]string{sessionCheckHeader: sessionCheckVersion, csrfHeader: body.CSRFToken})
			if pinned.StatusCode != http.StatusOK || *reached != before+1 || pinned.Header.Get(sessionCheckedHeader) != "v2 "+body.CredentialMarker+" "+itoa(alice.ID) {
				t.Fatalf("pinned beside the copies: %d %q", pinned.StatusCode, pinned.Header.Get(sessionCheckedHeader))
			}

			out, _ := b.send(srv, "POST", "/api/v1/auth/logout", nil, map[string]string{csrfHeader: body.CSRFToken})
			if out.StatusCode != http.StatusOK {
				t.Fatalf("logout: %d", out.StatusCode)
			}
			for path, want := range map[string][]string{
				"/api/v1/auth/bootstrap": {"fold-credential=copy-api"},
				"/groups/7":              {"fold-credential=copy-groups"},
				"/":                      nil,
			} {
				if got := b.markersAt(srv, path); !slices.Equal(got, want) {
					t.Fatalf("after logout the jar sends %v to %s, want %v", got, path, want)
				}
			}
		})
	}
}

// snapshotBegin is the BEGIN a participating reader's snapshot (readSnapshot)
// sends, which claims an armed proxy fault for that reader's connection.
const snapshotBegin = "isolation level repeatable read read only"

// snapshotRollbackFault routes app through the testdb proxy until t ends and
// returns a sender: it arms mode on the next snapshot's ROLLBACK, sends one
// request, and requires that exactly that one ROLLBACK was faulted.
func snapshotRollbackFault(t *testing.T, app *App) func(mode testdb.RollbackFault, send func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	var name string
	gormMust(t, app.DB.Raw("SELECT current_database()").Scan(&name))
	proxy := testdb.NewProxy(t)
	db, err := proxy.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	orig := app.DB
	app.DB = db
	t.Cleanup(func() { app.DB = orig })
	return func(mode testdb.RollbackFault, send func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		mark := len(proxy.Events())
		proxy.Arm(testdb.Faults{Match: snapshotBegin, Rollback: mode})
		w := send()
		proxy.Arm(testdb.Faults{Match: "never sent: disarmed"})
		want := testdb.ActionDropped
		if mode == testdb.RollbackForwardThenClose {
			want = testdb.ActionForwarded
		}
		var rollbacks []testdb.Event
		for _, e := range proxy.Events()[mark:] {
			if e.Kind == testdb.KindRollback {
				rollbacks = append(rollbacks, e)
			}
		}
		if len(rollbacks) != 1 || rollbacks[0].Action != want {
			t.Fatalf("faulted rollbacks %+v, want one %s (%d %s)", rollbacks, want, w.Code, w.Body.String())
		}
		return w
	}
}

// wantUnconfirmedRefusal checks that a refusal whose snapshot ROLLBACK went
// unconfirmed answers the generic 500 with no acknowledgment.
func wantUnconfirmedRefusal(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	wantNoAck(t, what+" with an unconfirmed rollback", w)
	if w.Code != http.StatusInternalServerError || w.Body.String() != serverErrorBody {
		t.Fatalf("%s with an unconfirmed rollback: %d %s", what, w.Code, w.Body.String())
	}
}

// TestSnapshotRefusalsNeedAConfirmedRollback: a participating reader's refusal
// (M8.1) is acknowledged only once its snapshot's ROLLBACK came back. When the
// ROLLBACK is lost or its answer is lost, the reader answers the generic 500
// with no acknowledgment. Missing and hidden resources answer identically.
func TestSnapshotRefusalsNeedAConfirmedRollback(t *testing.T) {
	modes := []struct {
		name string
		mode testdb.RollbackFault
	}{{"lost rollback", testdb.RollbackCloseUnforwarded}, {"lost answer", testdb.RollbackForwardThenClose}}
	// parity sends hidden and missing under each fault and requires the same
	// unconfirmed answer for both, after checking each is an acknowledged
	// refusal of status when its rollback is confirmed.
	parity := func(t *testing.T, fault func(testdb.RollbackFault, func() *httptest.ResponseRecorder) *httptest.ResponseRecorder, what string, c *apiClient, viewer *models.User, status int, paths ...string) {
		t.Helper()
		for _, path := range paths {
			if w := c.do("GET", path, nil, false); w.Code != status {
				t.Fatalf("%s %s: %d %s, want %d", what, path, w.Code, w.Body.String(), status)
			} else {
				wantAck(t, what+" "+path, w, c, viewer)
			}
		}
		for _, m := range modes {
			var bodies []string
			for _, path := range paths {
				w := fault(m.mode, func() *httptest.ResponseRecorder { return c.do("GET", path, nil, false) })
				wantUnconfirmedRefusal(t, what+" "+path+", "+m.name, w)
				bodies = append(bodies, w.Body.String())
			}
			for _, b := range bodies[1:] {
				if b != bodies[0] {
					t.Fatalf("%s, %s: bodies differ %q", what, m.name, bodies)
				}
			}
			if w := c.do("GET", paths[0], nil, false); w.Code != status {
				t.Fatalf("%s after the fault: %d %s", what, w.Code, w.Body.String())
			} else {
				wantAck(t, what+" after the fault", w, c, viewer)
			}
		}
	}

	t.Run("group reads", func(t *testing.T) {
		f := newGroupReadFixture(t, models.VisibilityInviteOnly)
		outsiderUser := newTestUser(t, f.app, "rollback outsider", "password1234", models.RoleMember)
		outsider := pinnedAs(t, loginAPI(t, f.app, outsiderUser.Name, "password1234"))
		anonPlain := newAPIClient(t, f.app)
		anonPlain.fetchCSRF()
		anon := pinnedAs(t, anonPlain)
		leader := pinnedAs(t, f.leader)
		fault := snapshotRollbackFault(t, f.app)
		for _, route := range groupReadRoutes {
			hidden, missing := groupReadPath(f.group, route.suffix), groupReadPath(f.group+100000, route.suffix)
			parity(t, fault, "an outsider's "+route.surface, outsider, outsiderUser, http.StatusNotFound, hidden, missing)
			if route.suffix != "/members" {
				parity(t, fault, "a visitor's "+route.surface, anon, nil, http.StatusNotFound, hidden, missing)
			}
		}
		parity(t, fault, "a malformed page", leader, f.leaderUser, http.StatusBadRequest, groupDetailPath(f.group, "cursor=not-a-cursor"))
	})

	t.Run("event page", func(t *testing.T) {
		f := newGroupReadFixture(t, models.VisibilityInviteOnly)
		outsiderUser := newTestUser(t, f.app, "rollback outsider", "password1234", models.RoleMember)
		outsider := pinnedAs(t, loginAPI(t, f.app, outsiderUser.Name, "password1234"))
		anonPlain := newAPIClient(t, f.app)
		anonPlain.fetchCSRF()
		anon := pinnedAs(t, anonPlain)
		fault := snapshotRollbackFault(t, f.app)
		hidden, missing := fmt.Sprintf("/api/v1/events/%d", f.event.ID), fmt.Sprintf("/api/v1/events/%d", f.event.ID+100000)
		parity(t, fault, "an outsider's event", outsider, outsiderUser, http.StatusNotFound, hidden, missing)
		parity(t, fault, "a visitor's event", anon, nil, http.StatusNotFound, hidden, missing)
	})

	t.Run("event visibility preview", func(t *testing.T) {
		f := newGroupReadFixture(t, models.VisibilityInviteOnly)
		outsiderUser := newTestUser(t, f.app, "rollback outsider", "password1234", models.RoleMember)
		outsider := pinnedAs(t, loginAPI(t, f.app, outsiderUser.Name, "password1234"))
		leader := pinnedAs(t, f.leader)
		fault := snapshotRollbackFault(t, f.app)
		const suffix = "/visibility-impact?visibility=unknown"
		hidden := "/api/v1/events/" + itoa(f.event.ID) + suffix
		missing := "/api/v1/events/" + itoa(f.event.ID+100000) + suffix
		parity(t, fault, "an outsider's preview", outsider, outsiderUser, http.StatusNotFound, hidden, missing)
		parity(t, fault, "an invalid preview", leader, f.leaderUser, http.StatusBadRequest, hidden)
	})

	t.Run("managed date", func(t *testing.T) {
		f := newDateEditFixture(t)
		stranger := pinnedAs(t, loginAPI(t, f.app, f.outsiders[0].Name, "password1234"))
		member, admin := pinnedAs(t, f.member), pinnedAs(t, f.admin)
		fault := snapshotRollbackFault(t, f.app)
		parity(t, fault, "a hidden or missing event", stranger, f.outsiders[0], http.StatusNotFound, managedDatePath(f.event.ID, f.date(0)), managedDatePath(999999, f.date(0)))
		parity(t, fault, "a member's managed date", member, f.memberUser, http.StatusForbidden, managedDatePath(f.event.ID, f.date(0)))
		parity(t, fault, "a missing date", admin, f.adminUser, http.StatusNotFound, managedDatePath(f.event.ID, "2001-01-01"))
	})

	t.Run("household", func(t *testing.T) {
		f := newHouseholdReadFixture(t)
		teen := pinnedAs(t, loginAPI(t, f.app, f.teen.Name, "password1234"))
		fault := snapshotRollbackFault(t, f.app)
		parity(t, fault, "a teen's household", teen, f.teen, http.StatusForbidden, f.householdPath)
	})
}
