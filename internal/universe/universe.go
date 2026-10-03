// Package universe builds the set of instruments the screen may pick from:
// NIFTY Midcap 150 and NIFTY Smallcap 250 constituents, less NIFTY 50 and
// NIFTY Next 50 members, less anything NSE lists under ASM or GSM or outside
// the normal (EQ) series. The constituent lists are NSE's public index CSVs,
// cached once per IST day.
package universe

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Index names the NSE index constituent files the universe is built from.
const (
	Midcap150   = "niftymidcap150"
	Smallcap250 = "niftysmallcap250"
	Nifty50     = "niftyfifty"
	NiftyNext50 = "niftynext50"
)

// indexFiles maps an index to its file under IndexBaseURL.
var indexFiles = map[string]string{
	Midcap150:   "ind_niftymidcap150list.csv",
	Smallcap250: "ind_niftysmallcap250list.csv",
	Nifty50:     "ind_nifty50list.csv",
	NiftyNext50: "ind_niftynext50list.csv",
}

// NSE endpoints. NSE's CDN refuses clients that do not present a browser
// User-Agent, so HTTPFetcher sends BrowserUserAgent.
const (
	IndexBaseURL = "https://nsearchives.nseindia.com/content/indices/"
	SiteBaseURL  = "https://www.nseindia.com"
	// BrowserUserAgent is sent to NSE.
	BrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// ist is the exchange's time zone; the cache is keyed by IST day.
var ist = time.FixedZone("IST", 5*3600+30*60)

// Stock is one constituent.
type Stock struct {
	Symbol string
	Name   string
	// Industry is NSE's macro industry for the stock; the screen treats it as
	// the stock's sector.
	Industry string
	ISIN     string
	// Series is the trading series: EQ is the normal rolling-settlement
	// series; BE, BZ and the like are trade-for-trade.
	Series string
	// Index is the index file the stock came from.
	Index string
}

// Key returns the stock's watchlist key, NSE:SYMBOL.
func (s Stock) Key() string { return "NSE:" + s.Symbol }

// Universe is the screen's candidate set.
type Universe struct {
	Stocks []Stock
	// Dropped says why each constituent that was removed was, by symbol.
	Dropped map[string]string
	// Notes are things the caller should show: a missing surveillance list.
	Notes []string
}

// ParseIndexCSV parses an NSE index constituent file:
//
//	Company Name,Industry,Symbol,Series,ISIN Code
//
// Columns are matched by header name.
func ParseIndexCSV(r io.Reader, index string) ([]Stock, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: header: %w", index, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))] = i
	}
	for _, name := range []string{"company name", "industry", "symbol", "series"} {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("%s: missing column %q", index, name)
		}
	}
	field := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []Stock
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", index, err)
		}
		sym := field(rec, "symbol")
		if sym == "" {
			continue
		}
		out = append(out, Stock{
			Symbol: sym, Name: field(rec, "company name"), Industry: field(rec, "industry"),
			ISIN: field(rec, "isin code"), Series: field(rec, "series"), Index: index,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no constituents", index)
	}
	return out, nil
}

// Fetcher downloads one NSE document by name; Load names index files and
// FetchSurveillance names "asm" and "gsm".
type Fetcher func(ctx context.Context, name string) (io.ReadCloser, error)

// HTTPFetcher fetches the index files from IndexBaseURL, and the ASM and GSM
// reports from SiteBaseURL, overridden by the two base URLs when non-empty.
func HTTPFetcher(client *http.Client, indexBase, siteBase string) Fetcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if indexBase == "" {
		indexBase = IndexBaseURL
	}
	if siteBase == "" {
		siteBase = SiteBaseURL
	}
	return func(ctx context.Context, name string) (io.ReadCloser, error) {
		var url string
		switch name {
		case "asm":
			url = strings.TrimRight(siteBase, "/") + "/api/reportASM"
		case "gsm":
			url = strings.TrimRight(siteBase, "/") + "/api/reportGSM"
		default:
			file, ok := indexFiles[name]
			if !ok {
				return nil, fmt.Errorf("unknown NSE document %q", name)
			}
			url = strings.TrimRight(indexBase, "/") + "/" + file
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", BrowserUserAgent)
		req.Header.Set("Accept", "text/csv,application/json,*/*")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch NSE %s: %w", name, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch NSE %s: HTTP %d", name, resp.StatusCode)
		}
		return resp.Body, nil
	}
}

// cached returns the cached document for the IST day of now, fetching and
// caching it first if there is none. The cache is dir/universe/date=DAY/name.
func cached(ctx context.Context, dir string, now time.Time, name, ext string, fetch Fetcher) (io.ReadCloser, error) {
	day := now.In(ist).Format(time.DateOnly)
	path := filepath.Join(dir, "universe", "date="+day, name+ext)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := download(ctx, path, name, fetch); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return os.Open(path)
}

// download writes through a temporary file, so an interrupted download never
// leaves a truncated cache file.
func download(ctx context.Context, path, name string, fetch Fetcher) error {
	body, err := fetch(ctx, name)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(body, 16<<20)); err != nil {
		tmp.Close()
		return fmt.Errorf("download NSE %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func loadIndex(ctx context.Context, dir string, now time.Time, index string, fetch Fetcher) ([]Stock, error) {
	r, err := cached(ctx, dir, now, index, ".csv", fetch)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return ParseIndexCSV(r, index)
}

// Load builds the universe for the IST day of now: Midcap 150 and Smallcap 250
// less Nifty 50 and Nifty Next 50, and less any stock not in the EQ series.
// Surveillance is applied separately (Universe.Exclude) because the lists can
// be unavailable without failing the screen.
func Load(ctx context.Context, cacheDir string, now time.Time, fetch Fetcher) (Universe, error) {
	u := Universe{Dropped: map[string]string{}}
	large := map[string]string{}
	for _, idx := range []string{Nifty50, NiftyNext50} {
		stocks, err := loadIndex(ctx, cacheDir, now, idx, fetch)
		if err != nil {
			return Universe{}, err
		}
		for _, s := range stocks {
			large[s.Symbol] = idx
		}
	}
	seen := map[string]bool{}
	for _, idx := range []string{Midcap150, Smallcap250} {
		stocks, err := loadIndex(ctx, cacheDir, now, idx, fetch)
		if err != nil {
			return Universe{}, err
		}
		for _, s := range stocks {
			switch {
			case seen[s.Symbol]:
			case large[s.Symbol] != "":
				u.Dropped[s.Symbol] = "member of " + large[s.Symbol]
			case s.Series != "EQ":
				u.Dropped[s.Symbol] = "series " + s.Series + " (trade-for-trade)"
			default:
				u.Stocks = append(u.Stocks, s)
			}
			seen[s.Symbol] = true
		}
	}
	return u, nil
}

// Exclude removes the stocks in reasons (symbol -> why) and records why.
func (u *Universe) Exclude(reasons map[string]string) {
	kept := u.Stocks[:0]
	for _, s := range u.Stocks {
		if why, ok := reasons[s.Symbol]; ok {
			u.Dropped[s.Symbol] = why
			continue
		}
		kept = append(kept, s)
	}
	u.Stocks = kept
}
