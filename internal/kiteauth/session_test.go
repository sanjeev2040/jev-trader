package kiteauth

import (
	"testing"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/config"
)

func TestExpiry(t *testing.T) {
	tests := []struct {
		name  string
		login time.Time
		want  time.Time
	}{
		{"morning login lasts until next 06:00", time.Date(2026, 10, 2, 8, 45, 0, 0, ist), time.Date(2026, 10, 3, 6, 0, 0, 0, ist)},
		{"late evening login", time.Date(2026, 10, 2, 23, 59, 0, 0, ist), time.Date(2026, 10, 3, 6, 0, 0, 0, ist)},
		{"before 06:00 expires the same morning", time.Date(2026, 10, 2, 5, 30, 0, 0, ist), time.Date(2026, 10, 2, 6, 0, 0, 0, ist)},
		{"exactly 06:00 lasts a full day", time.Date(2026, 10, 2, 6, 0, 0, 0, ist), time.Date(2026, 10, 3, 6, 0, 0, 0, ist)},
		{"month end", time.Date(2026, 10, 31, 9, 0, 0, 0, ist), time.Date(2026, 11, 1, 6, 0, 0, 0, ist)},
		// 2026-10-02 01:00 UTC is 06:30 IST on the same date.
		{"UTC input is converted to IST", time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 6, 0, 0, 0, ist)},
		// 2026-10-01 23:00 UTC is 04:30 IST on 2026-10-02.
		{"UTC previous day before IST 06:00", time.Date(2026, 10, 1, 23, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 6, 0, 0, 0, ist)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Expiry(tt.login); !got.Equal(tt.want) {
				t.Errorf("Expiry(%v) = %v, want %v", tt.login, got, tt.want)
			}
		})
	}
}

func TestSessionValid(t *testing.T) {
	expiry := time.Date(2026, 10, 3, 6, 0, 0, 0, ist)
	sess := Session{UserID: testUserID, AccessToken: config.NewSecret(testAccessToken), ExpiresAt: expiry}
	if !sess.Valid(expiry.Add(-time.Second)) {
		t.Error("session should be valid one second before expiry")
	}
	if sess.Valid(expiry) {
		t.Error("session should be invalid at expiry")
	}
	if (Session{ExpiresAt: expiry}).Valid(expiry.Add(-time.Hour)) {
		t.Error("session without a token should be invalid")
	}
}
