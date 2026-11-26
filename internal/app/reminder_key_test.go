package app

import "testing"

func TestReminderKeyRoundTripsAndMatchesItsSQL(t *testing.T) {
	k := reminderKey{Kind: reminderPushKind, Occ: 7, Start: 1700000000, Minutes: pushReminderMinutes}
	if got, ok := parseReminderKey(k.String()); !ok || got != k {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
	for _, bad := range []string{"", "going:1:2", "going:x:2:3", "going:1:2:3:4"} {
		if _, ok := parseReminderKey(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	var fromSQL string
	err := testApp(t).DB.Raw("SELECT " + reminderKeySQL("o", k.Kind, k.Minutes) +
		" FROM (SELECT 7::bigint AS id, to_timestamp(1700000000) AS starts_at) o").Scan(&fromSQL).Error
	if err != nil || fromSQL != k.String() {
		t.Fatalf("SQL builds %q (%v), Go builds %q", fromSQL, err, k.String())
	}
	job := reminderKey{Kind: kindReminderSend, Occ: 7, Start: 1700000000, Minutes: 1440}
	for _, kind := range []string{reminderGoing, reminderWaitlist} {
		message := reminderKey{Kind: kind, Occ: job.Occ, Start: job.Start, Minutes: job.Minutes}
		var jobFromSQL string
		err := testApp(t).DB.Raw("SELECT "+reminderJobKeySQL("m.reminder_key")+
			" FROM (SELECT ?::text AS reminder_key) m", message.String()).Scan(&jobFromSQL).Error
		if err != nil || jobFromSQL != job.String() {
			t.Fatalf("%s: SQL builds job key %q (%v), Go builds %q", kind, jobFromSQL, err, job.String())
		}
	}
}
