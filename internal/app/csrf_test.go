package app

import (
	"fold/models"
	"net/http"

	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestAPICSRFRequired(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "bob", "secret123", models.RoleMember)
	c := loginAPI(t, app, "bob", "secret123")

	w := c.do("POST", "/api/v1/groups", map[string]any{"name": "X"}, false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing csrf: got %d", w.Code)
	}
	w = c.do("POST", "/api/v1/groups", map[string]any{"name": "X"}, true)
	if w.Code == http.StatusForbidden {
		t.Fatalf("valid csrf rejected: %d", w.Code)
	}
}

func TestCSRFFailureIsJSON(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "bob", "password1234", models.RoleMember)
	c := loginAPI(t, app, "bob", "password1234")
	w := c.do("POST", "/api/v1/groups", map[string]any{"name": "X"}, false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("csrf: got %d", w.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	decodeJSON(t, w, &body)
	if body.Error == "" {
		t.Fatal("expected JSON error")
	}
}

// TestCSRFExemptRoutesAreExact sends every state-changing route a request
// with no CSRF token. Only the routes in csrfExemptRoutes may get past CSRF.
func TestCSRFExemptRoutesAreExact(t *testing.T) {
	app := testApp(t)
	newTestUser(t, app, "admin", "password12345", models.RoleAdmin) // get past /setup
	r := app.setupRouter()
	param := regexp.MustCompile(`[:*][A-Za-z]+`)

	var exempt []string
	for _, rt := range r.Routes() {
		if isSafeMethod(rt.Method) {
			continue
		}
		req := httptest.NewRequest(rt.Method, param.ReplaceAllString(rt.Path, "test"), nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if !strings.Contains(w.Body.String(), "CSRF token invalid") {
			exempt = append(exempt, rt.Method+" "+rt.Path)
		}
	}
	var want []string
	for _, e := range csrfExemptRoutes {
		if strings.Contains(e, " ") {
			want = append(want, e)
			continue
		}
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "CONNECT"} {
			want = append(want, m+" "+e)
		}
	}
	slices.Sort(exempt)
	slices.Sort(want)
	if !slices.Equal(exempt, want) {
		t.Fatalf("routes outside CSRF:\n got %v\nwant %v", exempt, want)
	}

	// The acceptance check in plain words.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/postmark/email", nil))
	if w.Code == http.StatusForbidden {
		t.Fatal("POST /hooks/postmark/email was rejected by CSRF")
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/auth/logout", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST /api/v1/auth/logout without a token = %d, want 403", w.Code)
	}
}

// csrfExemptRoutes are the only state-changing routes registered outside
// csrfMiddleware (setupRouter, app.go). TestCSRFExemptRoutesAreExact keeps it honest.
var csrfExemptRoutes = []string{"POST /hooks/postmark/email", "POST /u/unsubscribe/:token"}

// pinnedFixture is a participating route for the credential check (M8.1): it
// counts the requests that reached it and acknowledges the viewer it judged.
func pinnedFixture(t *testing.T, app *App) (*gin.Engine, *int) {
	t.Helper()
	r := app.setupRouter()
	reached := new(int)
	handler := func(c *gin.Context) {
		*reached++
		if c.GetBool(sessionPinnedKey) {
			if err := app.acknowledgeSession(c, apiUser(c)); err != nil {
				t.Error(err)
			}
		}
		c.JSON(http.StatusOK, gin.H{})
	}
	r.GET("/api/v1/test/pinned", app.optionalUser(), handler)
	r.POST("/api/v1/test/pinned", app.optionalUser(), handler)
	return r, reached
}

// sendPinned sends method with the jar's cookies, the given check header
// values and X-CSRF-Token values (none when nil).
func sendPinned(c *apiClient, method string, check, tokens []string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/test/pinned", nil)
	for _, ck := range c.jar {
		req.AddCookie(ck)
	}
	for _, v := range check {
		req.Header.Add(sessionCheckHeader, v)
	}
	for _, v := range tokens {
		req.Header.Add(csrfHeader, v)
	}
	w := httptest.NewRecorder()
	c.router.ServeHTTP(w, req)
	return w
}

// A participating request is judged on the T of the cookie it carried, on GET
// too, before any token is minted: a match reaches the handler and is
// acknowledged with the marker and viewer; every mismatch is the ordinary
// session_expired 403 with no Set-Cookie and no domain work. A plain request
// (no check header) behaves as before.
func TestPinnedCredentialCheck(t *testing.T) {
	app := testApp(t)
	alice := newTestUser(t, app, "alice", "password1234", models.RoleMember)
	r, reached := pinnedFixture(t, app)
	signedIn := loginAPI(t, app, "alice", "password1234")
	signedIn.router = r
	token, marker := credentialOf(t, signedIn)
	anon := newAPIClient(t, app)
	anon.router = r
	anon.fetchCSRF()
	anonToken, anonMarker := credentialOf(t, anon)
	legacy := newAPIClient(t, app)
	legacy.router = r
	legacy.jar[sessionName] = legacySessionCookie(t, app, alice)
	empty := newAPIClient(t, app)
	empty.router = r
	v2 := []string{sessionCheckVersion}

	for _, ok := range []struct {
		name   string
		c      *apiClient
		method string
		token  string
		ack    string
	}{
		{"signed-in GET", signedIn, "GET", token, "v2 " + marker + " " + itoa(alice.ID)},
		{"signed-in POST", signedIn, "POST", token, "v2 " + marker + " " + itoa(alice.ID)},
		{"anonymous GET", anon, "GET", anonToken, "v2 " + anonMarker + " anon"},
	} {
		before := *reached
		w := sendPinned(ok.c, ok.method, v2, []string{ok.token})
		if w.Code != http.StatusOK || *reached != before+1 || w.Header().Get(sessionCheckedHeader) != ok.ack || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatalf("%s: %d reached=%v ack=%q cookies=%v", ok.name, w.Code, *reached > before, w.Header().Get(sessionCheckedHeader), w.Header().Values("Set-Cookie"))
		}
	}

	for _, bad := range []struct {
		name   string
		c      *apiClient
		method string
		check  []string
		tokens []string
	}{
		{"another browser's T", signedIn, "GET", v2, []string{anonToken}},
		{"the replaced T after a swap", anon, "POST", v2, []string{token}},
		{"no token", signedIn, "GET", v2, nil},
		{"two tokens", signedIn, "GET", v2, []string{token, token}},
		{"an unknown check version", signedIn, "GET", []string{"v1"}, []string{token}},
		{"two check headers", signedIn, "GET", []string{"v2", "v2"}, []string{token}},
		{"no cookie", empty, "GET", v2, []string{token}},
		{"a cookie with no T", legacy, "GET", v2, []string{token}},
	} {
		before := *reached
		w := sendPinned(bad.c, bad.method, bad.check, bad.tokens)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"session_expired"`) {
			t.Fatalf("%s: %d %s", bad.name, w.Code, w.Body.String())
		}
		if *reached != before || len(w.Header().Values("Set-Cookie")) != 0 || w.Header().Get(sessionCheckedHeader) != "" {
			t.Fatalf("%s: reached the handler, set %v or acknowledged %q", bad.name, w.Header().Values("Set-Cookie"), w.Header().Get(sessionCheckedHeader))
		}
	}

	// Plain clients: no check header, so a GET needs no token and an old
	// cookie with no T is given one as before.
	if w := sendPinned(legacy, "GET", nil, nil); w.Code != http.StatusOK || w.Header().Get(sessionCheckedHeader) != "" || !setsCookie(w, sessionName) {
		t.Fatalf("plain GET with an old cookie: %d %v", w.Code, w.Header().Values("Set-Cookie"))
	}
	if w := sendPinned(signedIn, "POST", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("plain POST without a token: %d", w.Code)
	}
}

// legacySessionCookie is a signed-in cookie as an older release wrote it: the
// login keys and no CSRF token or marker.
func legacySessionCookie(t *testing.T, app *App, u *models.User) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	s, _ := app.Store.New(req, sessionName)
	s.Values[sessionUserKey] = u.ID
	s.Values[sessionVersionKey] = u.SessionVersion
	s.Values[sessionLogoutKey] = u.LogoutVersion
	w := httptest.NewRecorder()
	if err := app.Store.Save(req, w, s); err != nil {
		t.Fatal(err)
	}
	return w.Result().Cookies()[0]
}
