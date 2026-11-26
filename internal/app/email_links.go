package app

import (
	"strings"
)

// emailBaseURL is the absolute base for links in transactional email:
// SITE_URL in prod, localhost in dev.
func (a *App) emailBaseURL() string {
	if a.Config.SiteURL != "" {
		return strings.TrimSuffix(a.Config.SiteURL, "/")
	}
	return "http://localhost:" + a.Config.Port
}

func (a *App) emailLink(path string) string {
	return a.emailBaseURL() + path
}

const guestManagePath = "/guest/manage/"

func (a *App) guestManageURL(raw string) string {
	return a.emailLink(guestManagePath + raw)
}
