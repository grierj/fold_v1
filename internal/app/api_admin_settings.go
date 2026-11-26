package main

import (
	"errors"
	"net/http"
	"strings"

	"fold/internal/audit"
	"fold/internal/events"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type revisedSetting struct {
	key    string
	action string
}

var (
	churchTimezoneSetting = revisedSetting{settingTimezone, audit.TimezoneChanged}
	churchAddressSetting  = revisedSetting{settingChurchAddress, audit.ChurchAddressChanged}
	replyToSetting        = revisedSetting{settingReplyTo, audit.ReplyToChanged}
)

func (a *App) readAdminSnapshot(c *gin.Context, surface string, read func(*gorm.DB) error) error {
	var judged sessionJudgment
	err := decidingSnapshot(a.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		admin, err := lockAdminIn(tx, apiUser(c), actorRead)
		if err != nil {
			return err
		}
		judged.judge(admin)
		if a.hooks.readerAuthorized != nil {
			a.hooks.readerAuthorized(surface)
		}
		return read(tx)
	})
	if ackErr := a.acknowledgeDecision(c, judged, err == nil || decidedRefusal(err)); ackErr != nil {
		return ackErr
	}
	return err
}

func (a *App) readRevisedSetting(c *gin.Context, setting revisedSetting) (value, revision string, err error) {
	err = a.readAdminSnapshot(c, setting.key, func(tx *gorm.DB) error {
		value, revision, err = a.settingRevisionIn(tx, setting)
		return err
	})
	return
}

func (a *App) saveRevisedSetting(c *gin.Context, setting revisedSetting, value, given string, meta map[string]any) (string, error) {
	var revision string
	err := a.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		// A setting may not have a row yet. Lock its key before reading either
		// the admin or the setting, including for the first save.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?, hashtext(?))", adminSettingLockClass, setting.key).Error; err != nil {
			return err
		}
		actor, err := lockAdminIn(tx, apiUser(c), actorShare)
		if err != nil {
			return err
		}
		if err := a.checkSettingRevision(tx, setting, given); err != nil {
			return err
		}
		if err := writeAuditedSetting(tx, setting.key, value, audit.Entry{Actor: &actor.ID, Action: setting.action, Meta: meta, IP: c.ClientIP()}); err != nil {
			return err
		}
		_, revision, err = a.settingRevisionIn(tx, setting)
		return err
	}, readCommitted)
	return revision, err
}

func writeAuditedSetting(tx *gorm.DB, key, value string, entry audit.Entry) error {
	if err := setSettingIn(tx, key, value); err != nil {
		return err
	}
	entry.TargetType = "setting"
	return audit.Write(tx, entry)
}

func (a *App) saveSetting(c *gin.Context, key, value, action string, meta map[string]any) error {
	me := apiUser(c)
	return a.DB.Transaction(func(tx *gorm.DB) error {
		return writeAuditedSetting(tx, key, value, audit.Entry{Actor: &me.ID, Action: action, Meta: meta, IP: c.ClientIP()})
	})
}

func (a *App) apiAdminGetTimezone(c *gin.Context) {
	value, revision, err := a.readRevisedSetting(c, churchTimezoneSetting)
	if err != nil {
		fail(c, err)
		return
	}
	zone, err := churchZoneOf(value, value != "")
	if errors.Is(err, errBadChurchZone) {
		value = "UTC"
	} else if err != nil {
		fail(c, err)
		return
	} else {
		value = zone.String()
	}
	c.JSON(http.StatusOK, timezoneResponseJSON{Timezone: value, Revision: revision})
}

func (a *App) apiSetTimezone(c *gin.Context) {
	var body struct {
		Timezone string `json:"timezone"`
		Revision string `json:"revision"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, errTimezoneRequired)
		return
	}
	tz := strings.TrimSpace(body.Timezone)
	if _, err := events.LoadZone(tz); err != nil {
		fail(c, errInvalidTimezone)
		return
	}
	revision, err := a.saveRevisedSetting(c, churchTimezoneSetting, tz, body.Revision, nil)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, timezoneResponseJSON{Timezone: tz, Revision: revision})
}

type settingAddressBody struct {
	Address  *string `json:"address"`
	Revision string  `json:"revision"`
}

func (a *App) getSettingAddress(c *gin.Context, setting revisedSetting) {
	value, revision, err := a.readRevisedSetting(c, setting)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, settingAddressResponseJSON{Address: strings.TrimSpace(value), Revision: revision})
}

func (a *App) saveSettingAddress(c *gin.Context, setting revisedSetting, address, given string) {
	revision, err := a.saveRevisedSetting(c, setting, address, given, map[string]any{"set": address != ""})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, settingAddressResponseJSON{Address: address, Revision: revision})
}
