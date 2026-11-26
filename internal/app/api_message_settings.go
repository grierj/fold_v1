package app

import (
	"net/http"
	"strings"

	"fold/internal/audit"

	"github.com/gin-gonic/gin"
)

// The admin settings the send pipeline reads (M3.5b): guardian copies of
// messages to teens (DEC-19, off by default) and where replies to an
// announcement go (DEC-130). Each change is audited in its transaction.

var errReplyToInvalid = apiErr(http.StatusBadRequest, "reply_to_invalid", "enter one email address, or leave it empty")

func (a *App) apiAdminGetGuardianCopies(c *gin.Context) {
	c.JSON(http.StatusOK, settingOnResponseJSON{On: boolSettingIn(a.DB, settingGuardianCopies, false)})
}

func (a *App) apiAdminSetGuardianCopies(c *gin.Context) {
	var body struct {
		On *bool `json:"on"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.On == nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	value := "false"
	if *body.On {
		value = "true"
	}
	if err := a.saveSetting(c, settingGuardianCopies, value, audit.GuardianCopiesChanged, map[string]any{"on": *body.On}); err != nil {
		jsonError(c, http.StatusInternalServerError, "could not save the setting")
		return
	}
	c.JSON(http.StatusOK, settingOnResponseJSON{On: *body.On})
}

func (a *App) apiAdminGetReplyTo(c *gin.Context) {
	a.getSettingAddress(c, replyToSetting)
}

// apiAdminSetReplyTo sets where replies to announcements go: one plain
// address, usually the church office's, or empty for none (DEC-130).
func (a *App) apiAdminSetReplyTo(c *gin.Context) {
	var body settingAddressBody
	if err := c.ShouldBindJSON(&body); err != nil || body.Address == nil {
		jsonError(c, http.StatusBadRequest, "invalid request")
		return
	}
	addr := strings.TrimSpace(*body.Address)
	if addr != "" && !validEmail(addr) {
		fail(c, errReplyToInvalid)
		return
	}
	a.saveSettingAddress(c, replyToSetting, addr, body.Revision)
}
