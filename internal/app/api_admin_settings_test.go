package app

import (
	"net/http"
	"strings"
	"testing"

	"fold/internal/audit"
	"fold/models"
)

func TestAdminSettingsSavesNameTheirRead(t *testing.T) {
	for _, setting := range []struct {
		path, field, first, second, restored, action string
	}{
		{"timezone", "timezone", "America/Denver", "America/Boise", "UTC", audit.TimezoneChanged},
		{"settings/church-address", "address", "10 Main St", "20 Main St", "", audit.ChurchAddressChanged},
		{"settings/reply-to", "address", "first@example.com", "second@example.com", "", audit.ReplyToChanged},
	} {
		t.Run(setting.path, func(t *testing.T) {
			app := testApp(t)
			newTestUser(t, app, "first-admin", "password1234", models.RoleAdmin)
			newTestUser(t, app, "second-admin", "password1234", models.RoleAdmin)
			first := loginAPI(t, app, "first-admin", "password1234")
			second := loginAPI(t, app, "second-admin", "password1234")
			path := "/api/v1/admin/" + setting.path

			put := first.do("PUT", path, map[string]any{setting.field: setting.first}, true)
			if put.Code != http.StatusConflict || !strings.Contains(put.Body.String(), `"code":"setting_changed"`) {
				t.Fatalf("missing revision: %d %s", put.Code, put.Body.String())
			}
			if count := auditCount(t, app, setting.action); count != 0 {
				t.Fatalf("missing revision wrote %d audit rows", count)
			}
			bodyA := adminSettingBody(first, path, setting.field, setting.first)
			bodyB := adminSettingBody(second, path, setting.field, setting.second)
			if bodyA["revision"] != bodyB["revision"] {
				t.Fatal("the two admins read different revisions")
			}
			chA, chB := bgDo(first, "PUT", path, bodyA), bgDo(second, "PUT", path, bodyB)
			a, b := awaitResult(t, chA), awaitResult(t, chB)
			if a.code == http.StatusConflict {
				a, b = b, a
			}
			if a.code != http.StatusOK || b.code != http.StatusConflict || !strings.Contains(b.body, `"code":"setting_changed"`) {
				t.Fatalf("expected one winner: %+v %+v", a, b)
			}
			if count := auditCount(t, app, setting.action); count != 1 {
				t.Fatalf("expected one audit row, got %d", count)
			}
			if got := apiDo(t, first, "GET", path, nil, http.StatusOK); strings.TrimSpace(got) != strings.TrimSpace(a.body) {
				t.Fatalf("stored setting or revision differs from the winner: %s vs %s", got, a.body)
			}

			// Repeat after a row exists so the value goes away and back.
			apiDo(t, first, "PUT", path, adminSettingBody(first, path, setting.field, setting.restored), http.StatusOK)
			original := adminSettingBody(first, path, setting.field, setting.first)
			apiDo(t, second, "PUT", path, adminSettingBody(second, path, setting.field, setting.second), http.StatusOK)
			apiDo(t, second, "PUT", path, adminSettingBody(second, path, setting.field, setting.restored), http.StatusOK)
			apiDo(t, first, "PUT", path, original, http.StatusConflict)
			if count := auditCount(t, app, setting.action); count != 4 {
				t.Fatalf("stale replay wrote an audit row: got %d rows, want 4", count)
			}
		})
	}
}

func TestAdminRevisionReadsShareTheAuthoritySnapshot(t *testing.T) {
	for _, accounts := range []bool{false, true} {
		name := "setting"
		surface := settingReplyTo
		path := "/api/v1/admin/settings/reply-to"
		if accounts {
			name, surface, path = "accounts", "admin users", "/api/v1/admin/users"
		}
		t.Run(name, func(t *testing.T) {
			s := newHostScene(t)
			actor := pinnedAs(t, s.a)
			before := apiDo(t, actor, "GET", path, nil, http.StatusOK)
			demotion := adminEditBody(s.b, s.adminA.ID, map[string]string{"role": models.RoleMember})
			change := atReaderSeam(s.app, "authorized", surface, func() {
				apiDo(t, s.b, "PUT", "/api/v1/admin/users/"+itoa(s.adminA.ID), demotion, http.StatusOK)
				if !accounts {
					apiDo(t, s.b, "PUT", path, adminSettingBody(s.b, path, "address", "new@example.com"), http.StatusOK)
				}
			})
			w := actor.do("GET", path, nil, false)
			if !change.Load() || w.Code != http.StatusOK || w.Body.String() != before {
				t.Fatalf("read mixed the pre-demotion authority with later data: %d %s", w.Code, w.Body.String())
			}
			wantAck(t, path, w, actor, s.adminA)
			apiDo(t, actor, "GET", path, nil, http.StatusUnauthorized)
		})
	}
}
