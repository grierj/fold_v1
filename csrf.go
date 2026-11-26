package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// This file implements CSRF protection via the synchronizer token pattern:
// a random token is stored server-side in the (HMAC-signed) session cookie
// and must be submitted with every state-changing request, either as the
// "csrf_token" form field or the "X-CSRF-Token" header. Unlike header-based
// checks, this works identically over plain HTTP (local dev) and HTTPS
// (behind a TLS-terminating proxy).

const csrfFormField = "csrf_token"
const csrfHeader = "X-CSRF-Token"
const csrfSessionKey = "csrfToken"

// csrfContextKey holds the request's verified token for its handlers.
const csrfContextKey = "csrfToken"

func csrfField(token string) template.HTML {
	return template.HTML(fmt.Sprintf(
		`<input type="hidden" name="%s" value="%s">`,
		csrfFormField, template.HTMLEscapeString(token),
	))
}

func (a *App) csrfMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		session, err := a.Store.Get(c.Request, sessionName)
		// A participating request (M8.1) is judged on the cookie it carried,
		// before any token is minted: a mismatch is the ordinary stale-session
		// refusal, with no Set-Cookie and no replacement token.
		if _, pinned := c.Request.Header[sessionCheckHeader]; pinned {
			if err != nil || !pinnedCredentialMatches(c, session) {
				a.csrfFail(c, errSessionExpired)
				return
			}
			c.Set(sessionPinnedKey, true)
		}
		safe := isSafeMethod(c.Request.Method)
		if err != nil && !safe {
			a.csrfFail(c, errSessionExpired)
			return
		}

		token, _ := session.Values[csrfSessionKey].(string)
		if err != nil || token == "" {
			// Persist the new token so the cookie is set before any POST.
			if token, err = a.issueAnonymousCredential(c, session, err != nil); err != nil {
				if errors.Is(err, errCredentialEntropy) {
					a.csrfFail(c, errCSRFGenerate)
				} else {
					a.csrfFail(c, errCSRFSession)
				}
				return
			}
		}
		c.Set(csrfContextKey, token)

		if !safe {
			submitted := c.PostForm(csrfFormField)
			if submitted == "" {
				submitted = c.GetHeader(csrfHeader)
			}
			if subtle.ConstantTimeCompare([]byte(submitted), []byte(token)) != 1 {
				a.csrfFail(c, errSessionExpired)
				return
			}
		}
		c.Next()
	}
}

// The 500s the middleware can answer. The setup form shows their words as
// plain text; the API sends none of them (a 5xx message is replaced).
var (
	errCSRFSession  = apiErr(http.StatusInternalServerError, "", "Session error")
	errCSRFGenerate = apiErr(http.StatusInternalServerError, "", "Failed to generate CSRF token")
	errCSRFContact  = apiErr(http.StatusInternalServerError, "", "could not complete contact operation")
)

// csrfFail writes JSON for the API and plain text for the setup form.
func (a *App) csrfFail(c *gin.Context, refusal *apiError) {
	if err := a.auditRefusal(c, "csrf"); err != nil {
		refusal = errCSRFContact
	}
	switch {
	case !strings.HasPrefix(c.Request.URL.Path, "/api/"):
		c.String(refusal.Status, refusal.Message)
	case refusal.Status >= http.StatusInternalServerError:
		jsonError(c, refusal.Status, refusal.Message)
	default:
		fail(c, refusal)
	}
	c.Abort()
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}
