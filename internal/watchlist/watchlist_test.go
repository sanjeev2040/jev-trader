package watchlist

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample() File {
	return File{
		Date: "2026-10-05", GeneratedAt: time.Date(2026, 10, 5, 3, 10, 0, 0, time.UTC),
		Phase: PhaseFinal, Final: true, Direction: "long",
		Weights: map[string]float64{"news": 0.4},
		Entries: []Entry{
			{Rank: 1, Key: "NSE:KPITTECH", Symbol: "KPITTECH", Name: "KPIT Technologies", Sector: "Information Technology", Score: 0.5, Signals: map[string]float64{"news": 0.8}, Reasons: []string{"order win"}},
			{Rank: 2, Key: "NSE:BSOFT", Symbol: "BSOFT", Score: 0.2, Signals: map[string]float64{}},
		},
		Sources: []Source{{Name: "nse-index", Kind: "universe", Status: StatusOK, Items: 400}},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "watchlist"), "2026-10-05")
	want := sample()
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != Version || got.Date != want.Date || !got.Final || len(got.Entries) != 2 || got.Entries[0].Key != "NSE:KPITTECH" {
		t.Errorf("round trip = %+v", got)
	}
	if keys := strings.Join(got.Keys(), ","); keys != "NSE:KPITTECH,NSE:BSOFT" {
		t.Errorf("Keys = %s", keys)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*"))
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestLoadFallbackConditions(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "2026-10-05")
	if _, err := Load(path, time.Now()); !errors.Is(err, ErrMissing) {
		t.Errorf("missing file: err = %v, want ErrMissing", err)
	}
	if err := Write(path, sample()); err != nil {
		t.Fatal(err)
	}
	// 2026-10-05 03:40 UTC is 09:10 IST that day.
	if _, err := Load(path, time.Date(2026, 10, 5, 3, 40, 0, 0, time.UTC)); err != nil {
		t.Errorf("same IST day: %v", err)
	}
	// 2026-10-04 20:00 UTC is already 01:30 IST on the 5th.
	if _, err := Load(path, time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("IST day is the 5th at 20:00 UTC the 4th: %v", err)
	}
	if _, err := Load(path, time.Date(2026, 10, 6, 3, 40, 0, 0, time.UTC)); !errors.Is(err, ErrStale) {
		t.Errorf("next day: err = %v, want ErrStale", err)
	}
}

func TestReadRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	bad := map[string]string{
		"not json":      `{`,
		"unknown field": `{"version":1,"date":"2026-10-05","entries":[{"key":"NSE:A"}],"surprise":1}`,
		"no entries":    `{"version":1,"date":"2026-10-05","entries":[]}`,
		"bad key":       `{"version":1,"date":"2026-10-05","entries":[{"key":"INFY"}]}`,
		"duplicate":     `{"version":1,"date":"2026-10-05","entries":[{"key":"NSE:A"},{"key":"NSE:A"}]}`,
		"bad date":      `{"version":1,"date":"05-10-2026","entries":[{"key":"NSE:A"}]}`,
		"old version":   `{"version":0,"date":"2026-10-05","entries":[{"key":"NSE:A"}]}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path); err == nil {
				t.Error("Read accepted it")
			}
		})
	}
}

func TestWriteRefusesInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	f := sample()
	f.Entries = nil
	if err := Write(path, f); err == nil {
		t.Fatal("Write accepted an empty watchlist")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("an invalid watchlist was written")
	}
}
