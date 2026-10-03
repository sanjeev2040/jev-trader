package nsecal

import (
	"os"
	"strings"
	"testing"
)

const fixture = `
years_complete: [2026]
holidays:
  - {date: "2026-10-02", name: "Gandhi Jayanti"}
  - {date: "2027-01-26", name: "Republic Day", verified: false}
extra_sessions:
  - {date: "2026-11-07", name: "Special"}
`

func mustParse(t *testing.T, s string) *Calendar {
	t.Helper()
	c, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheck(t *testing.T) {
	c := mustParse(t, fixture)
	for _, tc := range []struct {
		date     string
		open     bool
		reason   string
		warnings bool
	}{
		{"2026-10-01", true, "regular", false},
		{"2026-10-02", false, "Gandhi Jayanti", false}, // weekday holiday
		{"2026-10-03", false, "weekend", false},
		{"2026-10-04", false, "weekend", false},
		{"2026-11-07", true, "extra session", false}, // Saturday session
		{"2027-01-26", false, "Republic Day", false},
		{"2027-01-27", true, "regular", true}, // year not complete: open with a warning
		{"2027-01-30", false, "weekend", false},
	} {
		got, err := c.Check(tc.date)
		if err != nil {
			t.Fatalf("%s: %v", tc.date, err)
		}
		if got.Open != tc.open || !strings.Contains(got.Reason, tc.reason) || (got.Warning != "") != tc.warnings {
			t.Errorf("%s: got %+v, want open=%v reason~%q warning=%v", tc.date, got, tc.open, tc.reason, tc.warnings)
		}
	}
	if _, err := c.Check("02-10-2026"); err == nil {
		t.Error("bad date accepted")
	}
}

func TestParseRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"weekend holiday":  `holidays: [{date: "2026-10-03", name: x}]`,
		"weekday extra":    `extra_sessions: [{date: "2026-10-01", name: x}]`,
		"bad date":         `holidays: [{date: "2026-13-01", name: x}]`,
		"duplicate":        `holidays: [{date: "2026-10-02", name: x}, {date: "2026-10-02", name: y}]`,
		"both":             "holidays: [{date: \"2026-11-06\", name: x}]\nextra_sessions: [{date: \"2026-11-06\", name: y}]",
		"unknown field":    `holiday: []`,
		"unquoted-bad-yml": `holidays: [`,
	} {
		if _, err := Parse(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The checked-in calendar must parse and agree with the 2026 facts the
// deployment report states: 16 weekday holidays, none on a weekend.
func TestRepoCalendar(t *testing.T) {
	f, err := os.Open("../../deploy/nse-holidays.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range c.Holidays {
		if strings.HasPrefix(h.Date, "2026-") {
			n++
		}
	}
	if n != 16 {
		t.Errorf("2026 holidays: got %d, want 16", n)
	}
	if r, _ := c.Check("2026-10-05"); !r.Open {
		t.Errorf("2026-10-05 (Monday) should be a trading day: %+v", r)
	}
	if r, _ := c.Check("2026-10-20"); r.Open {
		t.Errorf("2026-10-20 (Dussehra) should be closed: %+v", r)
	}
}
