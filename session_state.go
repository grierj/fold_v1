package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/sessions"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"fold/internal/policy"
	"fold/models"
)

// One home for what "this request's session is still current" means: the
// cookie keys, the one predicate, the one actor lock built on it, the one
// way to end every session of an account, and the one 401 answer.

// The session cookie's keys. A session holds the account and the two epochs
// the account's row must still carry (session_version: the credential epoch;
// logout_version: the sign-out epoch, M7.2).
const (
	sessionUserKey    = "userID"
	sessionVersionKey = "sessionVersion"
	sessionLogoutKey  = "logoutVersion"
)

func setSessionLogin(s *sessions.Session, u *models.User) {
	s.Values[sessionUserKey] = u.ID
	s.Values[sessionVersionKey] = u.SessionVersion
	s.Values[sessionLogoutKey] = u.LogoutVersion
}

func clearSessionLogin(s *sessions.Session) {
	delete(s.Values, sessionUserKey)
	delete(s.Values, sessionVersionKey)
	delete(s.Values, sessionLogoutKey)
}

// setSessionVersion moves this browser's login to the account's new
// credential epoch and leaves its logout epoch as it was, so a cookie that was
// signed out everywhere stays signed out (a verified email change).
func setSessionVersion(s *sessions.Session, version int) {
	s.Values[sessionVersionKey] = version
}

// The browser credential (M8.1). T is the session's CSRF token
// (csrfSessionKey), minted afresh with every credential replacement. Its
// marker M = base64url(HMAC-SHA256(deriveKey(credentialMarkerPurpose), T))
// rides in a host-only cookie JavaScript can read, so every tab sharing the
// cookie can see that the credential changed. M conveys no identity or
// authority; a participating request proves T itself (csrfMiddleware) and its
// answer names M and the viewer that decided it (acknowledgeSession).
const (
	credentialCookieSecure  = "__Host-fold-credential"
	credentialCookiePlain   = "fold-credential"
	credentialMarkerPurpose = "browser-credential-marker:v1"

	// A participating request sends sessionCheckHeader: sessionCheckVersion
	// and its pinned T as X-CSRF-Token, on reads too.
	sessionCheckHeader   = "Fold-Session-Check"
	sessionCheckedHeader = "Fold-Session-Checked"
	sessionCheckVersion  = "v2"
	// sessionPinnedKey marks a request whose T csrfMiddleware compared.
	sessionPinnedKey = "sessionPinned"
)

var (
	errCredentialEntropy = errors.New("could not mint a browser credential")
	errSessionWrite      = errors.New("could not write the session cookie")
)

// mintCredential is a fresh T: 32 random bytes, base64url.
func (a *App) mintCredential() (string, error) {
	b := make([]byte, 32)
	read := rand.Read
	if a.hooks.credentialEntropy != nil {
		read = a.hooks.credentialEntropy
	}
	if _, err := read(b); err != nil {
		return "", errCredentialEntropy
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// credentialMarker is M for the credential token.
func (a *App) credentialMarker(token string) (string, error) {
	if token == "" {
		return "", errCredentialEntropy
	}
	key, err := deriveKey(a.Config.SessionSecret, credentialMarkerPurpose)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// credentialCookieName is the marker cookie's name: __Host- needs Secure, so
// plain-HTTP dev uses the bare name.
func (a *App) credentialCookieName() string {
	if a.Store.Options.Secure {
		return credentialCookieSecure
	}
	return credentialCookiePlain
}

// otherCredentialCookieName is the name the other Secure setting uses. A
// deployment that switched between HTTP and HTTPS on one host leaves it in the
// jar beside the current one, and script that sees both trusts neither, so a
// marker write expires it whenever the request carries it.
func (a *App) otherCredentialCookieName() string {
	if a.Store.Options.Secure {
		return credentialCookiePlain
	}
	return credentialCookieSecure
}

// replaceCredential is the one credential replacement. change edits the login
// keys of this request's session and answers false when there is nothing to
// replace (another account's or a signed-out browser's cookie); otherwise a
// fresh T is bound in the same signed cookie and both cookies are written. T is
// minted before change runs, so a failed mint leaves the session untouched.
// The caller answers an error itself; after a database commit the browser
// bootstraps the credential it actually holds.
func (a *App) replaceCredential(c *gin.Context, change func(*sessions.Session) bool) error {
	session, err := a.Store.Get(c.Request, sessionName)
	if err != nil {
		return err
	}
	token, err := a.mintCredential()
	if err != nil {
		return err
	}
	if !change(session) {
		return nil
	}
	session.Values[csrfSessionKey] = token
	return a.writeSession(c, session)
}

// issueAnonymousCredential binds a first T to a session that has none (the
// lazy CSRF token of a first visit, or a cookie from before tokens existed).
func (a *App) issueAnonymousCredential(c *gin.Context, s *sessions.Session) (string, error) {
	token, err := a.mintCredential()
	if err != nil {
		return "", err
	}
	s.Values[csrfSessionKey] = token
	return token, a.writeSession(c, s)
}

// expireSession ends this browser's session and its marker (logout).
func (a *App) expireSession(c *gin.Context) error {
	session, _ := a.Store.Get(c.Request, sessionName)
	session.Values = map[any]any{}
	opts := *a.Store.Options
	opts.MaxAge = -1
	session.Options = &opts
	return a.writeSession(c, session)
}

// writeSession is the one writer of the session cookie. It encodes the session
// into a buffer and derives the marker first, so a failure writes neither; then
// it sets the marker for the session's T (expired when there is none or the
// session is being expired) and then the session cookie, replacing any earlier
// Set-Cookie for either name in this response.
func (a *App) writeSession(c *gin.Context, s *sessions.Session) error {
	var buf headerBuffer
	if err := a.Store.Save(c.Request, &buf, s); err != nil {
		return err
	}
	lines := buf.header.Values("Set-Cookie")
	if len(lines) != 1 {
		return errSessionWrite
	}
	marker := ""
	if token, _ := s.Values[csrfSessionKey].(string); token != "" && s.Options.MaxAge >= 0 {
		m, err := a.credentialMarker(token)
		if err != nil {
			return err
		}
		marker = m
	}
	dropSetCookies(c.Writer.Header(), sessionName, a.credentialCookieName(), a.otherCredentialCookieName())
	http.SetCookie(c.Writer, a.markerCookie(marker, s.Options))
	a.expireOtherMarker(c)
	c.Writer.Header().Add("Set-Cookie", lines[0])
	return nil
}

// writeMarker sets the marker for T alone, leaving the session cookie (and its
// expiry) as the browser holds it: bootstrap's repair of a missing marker or
// of a jar that still holds the other name.
func (a *App) writeMarker(c *gin.Context, marker string) {
	dropSetCookies(c.Writer.Header(), a.credentialCookieName(), a.otherCredentialCookieName())
	http.SetCookie(c.Writer, a.markerCookie(marker, a.Store.Options))
	a.expireOtherMarker(c)
}

// expireOtherMarker expires the other name, host-only at path /, when the
// request carries it. The line is always Secure: a browser refuses a __Host-
// line without it, and a secure page may expire a plain cookie. A browser that
// does not send a cookie (a Secure one to a plain-HTTP page, localhost aside)
// does not show it to that page's script either.
func (a *App) expireOtherMarker(c *gin.Context) {
	if _, err := c.Request.Cookie(a.otherCredentialCookieName()); err != nil {
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: a.otherCredentialCookieName(), Value: "", Path: "/", MaxAge: -1, Secure: true, SameSite: a.Store.Options.SameSite})
}

// markerCookie matches the session cookie's path, SameSite, Secure and
// lifetime, readable by script and host-only; an empty marker expires it.
func (a *App) markerCookie(marker string, opts *sessions.Options) *http.Cookie {
	o := *opts
	o.Domain = ""
	o.Path = "/"
	o.HttpOnly = false
	if marker == "" {
		o.MaxAge = -1
	}
	return sessions.NewCookie(a.credentialCookieName(), marker, &o)
}

// requestMarker is the one marker cookie the request carries, or "" when it
// has none, several, or the other name too. An ambiguous jar is never trusted:
// bootstrap sets the marker again and expires the other name at /, but a
// same-name copy at another path or domain is a cookie the server can neither
// tell apart nor name, so it stays (TestMarkerCopiesAtOtherPathsStayAndFailClosed).
func (a *App) requestMarker(c *gin.Context) string {
	found := ""
	n := 0
	for _, ck := range c.Request.Cookies() {
		switch ck.Name {
		case a.credentialCookieName():
			found = ck.Value
			n++
		case a.otherCredentialCookieName():
			return ""
		}
	}
	if n != 1 {
		return ""
	}
	return found
}

// pinnedCredentialMatches is the participating check: one v2 check header and
// one X-CSRF-Token equal, in constant time, to the T of the verified cookie.
func pinnedCredentialMatches(c *gin.Context, s *sessions.Session) bool {
	check := c.Request.Header.Values(sessionCheckHeader)
	pinned := c.Request.Header.Values(csrfHeader)
	stored, _ := s.Values[csrfSessionKey].(string)
	if len(check) != 1 || check[0] != sessionCheckVersion || len(pinned) != 1 || stored == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pinned[0]), []byte(stored)) == 1
}

// acknowledgeSession names, on a usable answer, the credential that was
// checked and the viewer the deciding read judged (nil is anonymous): "v2 <M>
// <id|anon>". M is derived from the verified T, never the readable cookie. A
// handler calls it only once its transaction's outcome is known.
func (a *App) acknowledgeSession(c *gin.Context, viewer *models.User) error {
	marker, err := a.credentialMarker(c.GetString(csrfContextKey))
	if err != nil {
		return err
	}
	who := "anon"
	if viewer != nil && viewer.ID != 0 {
		who = strconv.FormatUint(uint64(viewer.ID), 10)
	}
	c.Header(sessionCheckedHeader, sessionCheckVersion+" "+marker+" "+who)
	return nil
}

// sessionJudgment is the viewer a participating reader's deciding snapshot
// judged (snapshotViewerIn: nil is signed out) or its writer locked
// (lockActorIn). Only judge fills it, inside that transaction, so an answer is
// never acknowledged for the identity the middleware read before the decision.
type sessionJudgment struct {
	viewer *models.User
	judged bool
}

func (j *sessionJudgment) judge(viewer *models.User) { j.viewer, j.judged = viewer, true }

// acknowledgeDecision is the one acknowledgment of a participating reader or
// writer (M8.1), called once its transaction has ended and before the answer
// is written: decided is true for an answer that committed and for a refusal
// its transaction decided (decidedRefusal) whose rollback was confirmed (a
// reader's through decidingSnapshot, a writer's by rollbackConfirmed). A
// plain request, a viewer never judged and an outcome not decided get none,
// so the client treats the answer as unusable. A returned error is answered
// as a 500 with nothing of the decision. The answer says nothing about any
// other snapshot, earlier pages included.
func (a *App) acknowledgeDecision(c *gin.Context, j sessionJudgment, decided bool) error {
	if !c.GetBool(sessionPinnedKey) || !j.judged || !decided {
		return nil
	}
	return a.acknowledgeSession(c, j.viewer)
}

// decidingSnapshot runs read in one read-only Repeatable Read snapshot
// (readSnapshot) on db for a participating reader, so its outcome is known
// before acknowledgeDecision: nil once COMMIT came back, read's own error once
// its ROLLBACK did, and the COMMIT's error as it came. A ROLLBACK that did not
// come back on the live connection (lost, cut before its answer, or already
// done by a cancelled context) is a *snapshotRollbackError, which hides the
// refusal it ended, so it is never acknowledged and is answered as the generic
// 500. A panic in read rolls back and panics again.
func decidingSnapshot(db *gorm.DB, read func(tx *gorm.DB) error) error {
	tx := db.Begin(readSnapshot)
	if tx.Error != nil {
		return tx.Error
	}
	ended := false
	defer func() {
		if !ended {
			tx.Rollback()
		}
	}()
	err := read(tx)
	ended = true
	if err != nil {
		if rerr := tx.Rollback().Error; rerr != nil {
			return &snapshotRollbackError{cause: err, rollback: rerr}
		}
		return err
	}
	return tx.Commit().Error
}

// snapshotRollbackError: a participating reader's snapshot ended in an error
// and its ROLLBACK was not confirmed. Nothing unwraps it, so no caller reads
// the refusal it ended as an answer (decidedRefusal, fail, a 404's
// errors.Is); the cause is for logs only.
type snapshotRollbackError struct{ cause, rollback error }

func (e *snapshotRollbackError) Error() string {
	return fmt.Sprintf("%v (snapshot rollback not confirmed: %v)", e.cause, e.rollback)
}

// decidedRefusal is an error that is a reader's or writer's answer: a 4xx the
// request can act on, never the 401 (errSecuritySession) and never a failure.
func decidedRefusal(err error) bool {
	if err == nil || errors.Is(err, errSecuritySession) {
		return false
	}
	var ae *apiError
	var an answerer
	switch {
	case errors.As(err, &ae):
	case errors.As(err, &an):
		ae = an.apiAnswer()
	default:
		return false
	}
	return ae.Status >= http.StatusBadRequest && ae.Status < http.StatusInternalServerError && ae.Status != http.StatusUnauthorized
}

// headerBuffer captures what the cookie store would write, so writeSession can
// check both cookies before either reaches the response.
type headerBuffer struct{ header http.Header }

func (b *headerBuffer) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}
func (b *headerBuffer) Write(p []byte) (int, error) { return len(p), nil }
func (b *headerBuffer) WriteHeader(int)             {}

// dropSetCookies removes this response's Set-Cookie lines for the names.
func dropSetCookies(h http.Header, names ...string) {
	var kept []string
	for _, line := range h.Values("Set-Cookie") {
		name, _, _ := strings.Cut(line, "=")
		drop := false
		for _, n := range names {
			drop = drop || strings.TrimSpace(name) == n
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	h.Del("Set-Cookie")
	for _, line := range kept {
		h.Add("Set-Cookie", line)
	}
}

// sessionCurrent is the one decision that a session may still act: fresh is
// the account's row as read now (live or under a lock), session is the user as
// the request authenticated it (only ID, SessionVersion and LogoutVersion are
// read). The row must be there, not soft deleted, able to sign in (an active
// account with a password) and carry both of the session's epochs. Every
// recheck of a signed-in actor, from the middleware to the actor seal, decides
// with this function, so a new epoch is added here and nowhere else.
func sessionCurrent(fresh, session *models.User) bool {
	return fresh != nil && session != nil && fresh.ID != 0 && fresh.ID == session.ID &&
		!fresh.DeletedAt.Valid && fresh.CanSignIn() &&
		fresh.SessionVersion == session.SessionVersion && fresh.LogoutVersion == session.LogoutVersion
}

// adminCurrent is sessionCurrent for a request that needs an admin: the same
// decision plus the row's own role, for the sites that hold the actor in a
// batch of locked rows (or lock it themselves) and refuse with their own error.
func adminCurrent(fresh, session *models.User) bool {
	return sessionCurrent(fresh, session) && policy.IsAdmin(asPerson(fresh))
}

// actorLock is how lockActorIn reads the actor's row. The set is small and
// fixed; each site keeps the strength it had, because the lock order and the
// deadlock analysis of the transaction depend on it.
type actorLock int

const (
	// actorRead is a plain read, for a transaction in which a lock on the
	// actor could cycle with the actor's own ungated change (the contact gate
	// serializes the rest), or that reads one snapshot.
	actorRead actorLock = iota
	// actorShare takes the row FOR SHARE: no change to the actor until commit.
	actorShare
	// actorNoKeyUpdate takes the row FOR NO KEY UPDATE, the user lock of the
	// writers that go on to change the account's own rows.
	actorNoKeyUpdate
)

func (m actorLock) clause() []clause.Expression {
	switch m {
	case actorShare:
		return []clause.Expression{clause.Locking{Strength: "SHARE"}}
	case actorNoKeyUpdate:
		return []clause.Expression{clause.Locking{Strength: "NO KEY UPDATE"}}
	}
	return nil
}

// lockActorIn reads the signed-in actor's own row (locked as mode says) and
// returns it when the session is still current. A missing or soft-deleted row
// and a stale session both answer errSecuritySession and the caller writes
// nothing; any other error is the database's. The caller uses the returned
// row, never the preloaded one, for every later fact.
func lockActorIn(tx *gorm.DB, session *models.User, mode actorLock) (*models.User, error) {
	var fresh models.User
	if err := tx.Clauses(mode.clause()...).First(&fresh, session.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errSecuritySession
		}
		return nil, err
	}
	if !sessionCurrent(&fresh, session) {
		return nil, errSecuritySession
	}
	return &fresh, nil
}

// lockAdminIn is lockActorIn for a writer that also needs the actor to be an
// admin on the locked row. A current non-admin answers errSecuritySession too:
// every admin route is behind the admin middleware and a role change bumps the
// session version, so the two are the same stale request.
func lockAdminIn(tx *gorm.DB, session *models.User, mode actorLock) (*models.User, error) {
	fresh, err := lockActorIn(tx, session, mode)
	if err != nil {
		return nil, err
	}
	if !adminCurrent(fresh, session) {
		return nil, errSecuritySession
	}
	return fresh, nil
}

// sessionVersionBump is the column value that ends every login and every
// emailed or pushed capability bound to the credential epoch.
func sessionVersionBump() clause.Expr { return gorm.Expr("session_version + 1") }

// endAllSessionsIn ends every session of the account in the caller's
// transaction, after the contact gate and the user lock: it sets set (columns
// that change with it, such as the new email or password hash) and bumps the
// session version in one UPDATE of user's row, then revokes every outstanding
// account capability. Writers that bump the epoch without revoking tokens (a
// re-enabled account keeps the setup link it was sent) use sessionVersionBump
// directly.
func endAllSessionsIn(tx *gorm.DB, user *models.User, set map[string]any) error {
	updates := map[string]any{"session_version": sessionVersionBump()}
	for k, v := range set {
		updates[k] = v
	}
	if err := tx.Model(user).Updates(updates).Error; err != nil {
		return err
	}
	return revokeAccountTokens(tx, user.ID)
}

func answerStaleSession(c *gin.Context, err error) bool {
	if !errors.Is(err, errSecuritySession) {
		return false
	}
	fail(c, errSecuritySession)
	return true
}

// sessionVersion reads the password generation stored at login.
// A cookie issued before this field existed has no value and counts as 0.
func sessionVersion(s *sessions.Session) int { return sessionInt(s, sessionVersionKey) }

// logoutVersion reads the logout generation stored at login (M7.2); an
// older cookie has none and counts as 0, the column's default.
func logoutVersion(s *sessions.Session) int { return sessionInt(s, sessionLogoutKey) }

func sessionInt(s *sessions.Session, key string) int {
	switch v := s.Values[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case uint:
		return int(v)
	default:
		return 0
	}
}
