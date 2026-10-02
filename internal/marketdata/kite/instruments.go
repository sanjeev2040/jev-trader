package kite

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// DefaultAPIURL is the Kite Connect REST root.
const DefaultAPIURL = "https://api.kite.trade"

// ist is the exchange's time zone.
var ist = time.FixedZone("IST", 5*3600+1800)

// ParseInstruments parses a Kite instrument master CSV dump. Columns are
// matched by header name, so column order does not matter.
func ParseInstruments(r io.Reader) ([]domain.Instrument, error) {
	cr := csv.NewReader(r)
	cr.ReuseRecord = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("instrument master header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, name := range []string{"instrument_token", "tradingsymbol", "exchange"} {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("instrument master: missing column %q", name)
		}
	}
	field := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}

	var out []domain.Instrument
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("instrument master: %w", err)
		}
		line, _ := cr.FieldPos(0)
		token, err := strconv.ParseUint(field(rec, "instrument_token"), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("instrument master line %d: instrument_token: %w", line, err)
		}
		in := domain.Instrument{
			Token:    uint32(token),
			Exchange: field(rec, "exchange"),
			Symbol:   field(rec, "tradingsymbol"),
		}
		out = append(out, in)
	}
}

// instrumentKey returns the instrument's watchlist key, "EXCHANGE:SYMBOL".
func instrumentKey(in domain.Instrument) string { return in.Exchange + ":" + in.Symbol }

// Resolve maps watchlist entries to instruments. An entry must be exactly an
// instrument's "EXCHANGE:SYMBOL" key. It fails listing every entry that is
// malformed or not in the master.
func Resolve(master []domain.Instrument, watchlist []string) ([]domain.Instrument, error) {
	if err := checkEntries(watchlist); err != nil {
		return nil, err
	}
	byKey := make(map[string]domain.Instrument, len(master))
	for _, in := range master {
		byKey[instrumentKey(in)] = in
	}
	var (
		out     []domain.Instrument
		missing []string
		seen    = map[uint32]bool{}
	)
	for _, entry := range watchlist {
		in, ok := byKey[entry]
		if !ok {
			missing = append(missing, entry)
			continue
		}
		if !seen[in.Token] {
			seen[in.Token] = true
			out = append(out, in)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("watchlist symbols not in instrument master: %s", strings.Join(missing, ", "))
	}
	if len(out) == 0 {
		return nil, errors.New("watchlist is empty")
	}
	return out, nil
}

// MasterFetcher downloads the instrument master CSV for one exchange.
type MasterFetcher func(ctx context.Context, exchange string) (io.ReadCloser, error)

// HTTPMasterFetcher fetches the instrument master from the Kite REST API
// (GET /instruments/{exchange}). baseURL defaults to DefaultAPIURL and client
// to http.DefaultClient.
func HTTPMasterFetcher(client *http.Client, baseURL, apiKey string, tokens TokenProvider) MasterFetcher {
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = DefaultAPIURL
	}
	return func(ctx context.Context, exchange string) (io.ReadCloser, error) {
		token, err := tokens.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("kite access token: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/instruments/"+exchange, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Kite-Version", "3")
		req.Header.Set("Authorization", "token "+apiKey+":"+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch instrument master: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch instrument master: HTTP %d", resp.StatusCode)
		}
		return resp.Body, nil
	}
}

// masterCutover is how long after IST midnight Kite regenerates the
// instrument master.
const masterCutover = 8*time.Hour + 30*time.Minute

// LoadMaster returns the instrument master for the given exchanges, loading
// it at most once per master day. A master day starts at the ~08:30 IST
// regeneration, so before then the previous day's dump is still current. Each
// exchange's dump is cached as cacheDir/instruments/date=YYYY-MM-DD/EXCHANGE.csv;
// a cached file for the master day of now is reused instead of fetching again.
func LoadMaster(ctx context.Context, cacheDir string, now time.Time, exchanges []string, fetch MasterFetcher) ([]domain.Instrument, error) {
	day := now.In(ist).Add(-masterCutover).Format(time.DateOnly)
	dir := filepath.Join(cacheDir, "instruments", "date="+day)
	var all []domain.Instrument
	for _, ex := range exchanges {
		path := filepath.Join(dir, ex+".csv")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if err := downloadMaster(ctx, path, ex, fetch); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		ins, err := ParseInstruments(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, ins...)
	}
	return all, nil
}

// downloadMaster writes the dump to a temporary file and renames it into
// place, so an interrupted download never leaves a truncated cache file.
func downloadMaster(ctx context.Context, path, exchange string, fetch MasterFetcher) error {
	body, err := fetch(ctx, exchange)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+exchange+"-*.csv.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return fmt.Errorf("download instrument master %s: %w", exchange, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Exchanges returns the distinct exchanges named by watchlist entries, in
// first-seen order. Every entry must be in "EXCHANGE:SYMBOL" form.
func Exchanges(watchlist []string) ([]string, error) {
	if err := checkEntries(watchlist); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, entry := range watchlist {
		ex, _, _ := strings.Cut(entry, ":")
		if !seen[ex] {
			seen[ex] = true
			out = append(out, ex)
		}
	}
	return out, nil
}

// checkEntries fails naming every watchlist entry not in "EXCHANGE:SYMBOL"
// form.
func checkEntries(watchlist []string) error {
	var bad []string
	for _, entry := range watchlist {
		ex, sym, ok := strings.Cut(entry, ":")
		if !ok || ex == "" || sym == "" {
			bad = append(bad, fmt.Sprintf("%q", entry))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("watchlist entries not in EXCHANGE:SYMBOL form: %s", strings.Join(bad, ", "))
	}
	return nil
}
