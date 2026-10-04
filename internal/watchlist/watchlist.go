// Package watchlist is the dated file `jevtrader screen` writes before the
// session and `run` and `replay` read: the instruments to trade today, with the
// scores and reasons that put them there. See reports/0004-watchlist.md.
package watchlist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IST is the exchange's time zone; a watchlist's Date is an IST day.
var IST = time.FixedZone("IST", 5*3600+30*60)

// Version is the file format version.
const Version = 1

// Phases of a screen. Early runs after the VM starts, before pre-open, from the
// news and the previous session. Final adds the pre-open gap. Historic is a
// rerun for a past day, which has no pre-open to read.
const (
	PhaseEarly    = "early"
	PhaseFinal    = "final"
	PhaseHistoric = "historic"
)

// Errors Load returns; run falls back to the static watchlist on either.
var (
	ErrMissing = errors.New("watchlist file not found")
	ErrStale   = errors.New("watchlist file is not for today")
)

// File is the watchlist file.
type File struct {
	Version     int       `json:"version"`
	Date        string    `json:"date"` // the IST trading day, YYYY-MM-DD
	GeneratedAt time.Time `json:"generated_at"`
	Phase       string    `json:"phase"`
	// Final is true when no later screen is expected to refine this file.
	Final bool `json:"final"`
	// Model is the pinned Jev model that scored the news; empty if none did.
	Model   string             `json:"model,omitempty"`
	Weights map[string]float64 `json:"weights"`
	// Direction is "long" (rank by expected rise) or "both" (by |score|).
	Direction string   `json:"direction"`
	Sectors   []Sector `json:"sectors"`
	Entries   []Entry  `json:"entries"`
	Sources   []Source `json:"sources"`
	Notes     []string `json:"notes,omitempty"`
	// Pool is every stock that passed the liquidity filters, with its score
	// and signals, when the screen was asked to keep it (the picker study
	// draws its comparison list from it). Entries is a subset of it.
	Pool []PoolEntry `json:"pool,omitempty"`
}

// PoolEntry is one screened stock, picked or not.
type PoolEntry struct {
	Key     string             `json:"key"` // EXCHANGE:SYMBOL
	Sector  string             `json:"sector"`
	Score   float64            `json:"score"`
	Signals map[string]float64 `json:"signals"`
	// NewsItems is how many news items Jev scored about the stock, so a stock
	// whose items netted to zero can be told from one with none.
	NewsItems int `json:"news_items,omitempty"`
}

// Sector is a group of the universe and its strength.
type Sector struct {
	Name    string `json:"name"`
	Members int    `json:"members"`
	// Score is the sector signal in [-1, 1].
	Score float64 `json:"score"`
	// PrevReturn and Gap are the members' mean previous-session return and
	// mean pre-open gap, in percent; Gap is nil before pre-open.
	PrevReturn float64  `json:"prev_return_pct"`
	Gap        *float64 `json:"gap_pct,omitempty"`
	// Index is the sectoral index quoted for this sector, if one was.
	Index       string   `json:"index,omitempty"`
	IndexChange *float64 `json:"index_change_pct,omitempty"`
}

// Entry is one instrument to trade.
type Entry struct {
	Rank   int    `json:"rank"`
	Key    string `json:"key"` // EXCHANGE:SYMBOL, as run's --watchlist takes it
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
	Sector string `json:"sector"`
	// Score is the weighted sum of the Signals, in [-1, 1].
	Score float64 `json:"score"`
	// Signals are the normalised signals in [-1, 1] that were available.
	Signals map[string]float64 `json:"signals"`
	Reasons []string           `json:"reasons"`
	News    []NewsRef          `json:"news,omitempty"`
}

// NewsRef is a news item behind an entry, with what Jev made of it.
type NewsRef struct {
	Source    string    `json:"source"`
	Title     string    `json:"title"`
	URL       string    `json:"url,omitempty"`
	Published time.Time `json:"published"`
	// Impact is Jev's expected-impact level (0..4), Material the probability
	// the item is material and price-moving, Direction the net probability
	// the price rises (+1) rather than falls (-1).
	Impact    float64 `json:"impact"`
	Material  float64 `json:"material"`
	Direction float64 `json:"direction"`
}

// Source says what one data source contributed.
type Source struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`   // universe, surveillance, news, market, jev
	Status string `json:"status"` // ok, failed or skipped
	Items  int    `json:"items"`
	Detail string `json:"detail,omitempty"`
	// InputTokens and OutputTokens are what the source's Jev calls cost in
	// this run; calls answered from the day's cache are not counted.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// Status values of a Source.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// Keys returns the entries' EXCHANGE:SYMBOL keys in rank order.
func (f File) Keys() []string {
	keys := make([]string, len(f.Entries))
	for i, e := range f.Entries {
		keys[i] = e.Key
	}
	return keys
}

// Validate checks the file is one run can trade from.
func (f File) Validate() error {
	if f.Version != Version {
		return fmt.Errorf("watchlist version %d, want %d", f.Version, Version)
	}
	if _, err := time.ParseInLocation(time.DateOnly, f.Date, IST); err != nil {
		return fmt.Errorf("watchlist date %q: %w", f.Date, err)
	}
	seen := map[string]bool{}
	for i, e := range f.Entries {
		ex, sym, ok := strings.Cut(e.Key, ":")
		if !ok || ex == "" || sym == "" {
			return fmt.Errorf("watchlist entry %d: key %q is not EXCHANGE:SYMBOL", i, e.Key)
		}
		if seen[e.Key] {
			return fmt.Errorf("watchlist entry %d: %s is listed twice", i, e.Key)
		}
		seen[e.Key] = true
	}
	return nil
}

// Path returns where the watchlist for date (YYYY-MM-DD) lives under dir.
func Path(dir, date string) string { return filepath.Join(dir, date+".json") }

// Write validates f and writes it to path atomically: a reader sees the whole
// old file or the whole new one, never a partial write.
func Write(path string, f File) error {
	f.Version = Version
	if err := f.Validate(); err != nil {
		return err
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Read parses and validates the file at path. A missing file is ErrMissing.
func Read(path string) (File, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, fmt.Errorf("%w: %s", ErrMissing, path)
	}
	if err != nil {
		return File{}, err
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// Load reads path and requires it to be the watchlist for the IST day of now.
// A missing file is ErrMissing and one for another day is ErrStale; the caller
// then uses its static watchlist.
func Load(path string, now time.Time) (File, error) {
	f, err := Read(path)
	if err != nil {
		return File{}, err
	}
	if today := now.In(IST).Format(time.DateOnly); f.Date != today {
		return File{}, fmt.Errorf("%w: %s is for %s, today is %s", ErrStale, path, f.Date, today)
	}
	return f, nil
}
