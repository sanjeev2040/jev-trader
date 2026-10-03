package news

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NSEBaseURL is NSE's website, which serves the announcements API.
const NSEBaseURL = "https://www.nseindia.com"

// nseBrowserUserAgent: NSE's CDN refuses clients without a browser
// User-Agent (verified 2026-10-03), as it does for the index files.
const nseBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// DefaultNoiseCategories are announcement categories that are routine
// paperwork and never worth a model call.
var DefaultNoiseCategories = []string{
	"Shareholders meeting",
	"Copy of Newspaper Publication",
	"Trading Window",
	"Certificate under SEBI (Depositories and Participants) Regulations, 2018",
	"ESOP/ESOS/ESPS",
	"Allotment of Securities",
	"Options to purchase securities",
}

// NSEAnnouncements reads NSE's corporate announcements
// (GET /api/corporate-announcements?index=equities&from_date=DD-MM-YYYY&to_date=DD-MM-YYYY),
// the filings every listed company must make to the exchange.
type NSEAnnouncements struct {
	BaseURL string
	Client  *http.Client
	// Skip holds categories (the "desc" field) to leave out, matched
	// case-insensitively; nil means DefaultNoiseCategories.
	Skip []string
}

func (NSEAnnouncements) Name() string { return "nse-announcements" }

type nseRow struct {
	Symbol   string `json:"symbol"`
	Desc     string `json:"desc"`
	Text     string `json:"attchmntText"`
	AnDt     string `json:"an_dt"`
	SeqID    string `json:"seq_id"`
	Company  string `json:"sm_name"`
	Industry string `json:"smIndustry"`
	File     string `json:"attchmntFile"`
}

const nseTimeLayout = "02-Jan-2006 15:04:05"

// Fetch returns announcements made in [from, to).
func (n NSEAnnouncements) Fetch(ctx context.Context, from, to time.Time) ([]Item, error) {
	base := n.BaseURL
	if base == "" {
		base = NSEBaseURL
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	q := url.Values{
		"index":     {"equities"},
		"from_date": {from.In(IST).Format("02-01-2006")},
		"to_date":   {to.Add(-time.Second).In(IST).Format("02-01-2006")},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/corporate-announcements?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", nseBrowserUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nse-announcements: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nse-announcements: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("nse-announcements: %w", err)
	}
	return n.parse(body, from, to)
}

func (n NSEAnnouncements) parse(body []byte, from, to time.Time) ([]Item, error) {
	var rows []nseRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("nse-announcements: unexpected response: %w", err)
	}
	skip := n.Skip
	if skip == nil {
		skip = DefaultNoiseCategories
	}
	skipped := make(map[string]bool, len(skip))
	for _, c := range skip {
		skipped[strings.ToLower(strings.TrimSpace(c))] = true
	}
	var out []Item
	for _, r := range rows {
		at, err := time.ParseInLocation(nseTimeLayout, r.AnDt, IST)
		if err != nil || at.Before(from) || !at.Before(to) || r.Symbol == "" {
			continue
		}
		if skipped[strings.ToLower(strings.TrimSpace(r.Desc))] {
			continue
		}
		text := strings.Join(strings.Fields(r.Text), " ")
		if text == "" {
			text = r.Desc
		}
		out = append(out, Item{
			Source: "nse-announcements", ID: r.SeqID, Symbol: r.Symbol,
			Title: text, Category: r.Desc, URL: r.File, Published: at,
		})
	}
	return out, nil
}
