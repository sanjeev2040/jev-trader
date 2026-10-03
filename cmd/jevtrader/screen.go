package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/marketdata/kite"
	"github.com/sanjeev2040/jev-trader/internal/news"
	"github.com/sanjeev2040/jev-trader/internal/screen"
	"github.com/sanjeev2040/jev-trader/internal/universe"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

// defaultWatchlistDir is where screen writes, and where run looks.
const defaultWatchlistDir = "state/watchlist"

// preOpenReadFrom is when the pre-open order book has formed enough for the
// indicative price to mean something: the call auction runs 09:00-09:08 and
// the equilibrium price is set at 09:08.
const preOpenReadFrom = 9*time.Hour + 8*time.Minute

// rssFeeds are the default RSS feeds, each verified to serve RSS 2.0 to this
// client on 2026-10-03 (reports/0004-watchlist.md). Business Standard's feeds
// answer 403 to anything but a browser and are left out rather than
// impersonated.
var rssFeeds = []struct{ name, url string }{
	{"economictimes-stocks", "https://economictimes.indiatimes.com/markets/stocks/rssfeeds/2146842.cms"},
	{"economictimes-markets", "https://economictimes.indiatimes.com/markets/rssfeeds/1977021501.cms"},
	{"livemint-markets", "https://www.livemint.com/rss/markets"},
	{"thehindubusinessline-markets", "https://www.thehindubusinessline.com/markets/feeder/default.rss"},
	{"ndtvprofit", "https://feeds.feedburner.com/ndtvprofit-latest"},
}

const nseAnnouncementsName = "nse-announcements"

func runScreen(ctx context.Context, args []string, e env) int {
	if err := screenCmd(ctx, args, e); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(e.stderr, "jevtrader screen: %v\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

// screenOptions are what the flags say beyond screen.Config.
type screenOptions struct {
	dataDir, stateDir, model, jevURL, kiteURL string
	sources                                   []string
}

// buildScreenDeps connects the screen to NSE, Kite, the news sources and Jev.
// Tests replace it with fixtures.
var buildScreenDeps = realScreenDeps

func screenCmd(ctx context.Context, args []string, e env) error {
	fs := flag.NewFlagSet("screen", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	def := screen.DefaultConfig()
	date := fs.String("date", "", "IST trading day to screen for, YYYY-MM-DD (default: today). A past day is rebuilt from the candles and filings that were public before its open")
	phase := fs.String("phase", "auto", "auto, early or final. early (before pre-open) uses news and the previous session; final adds the pre-open gap. auto: early before 09:08 IST, final from 09:08; a past --date is historic")
	outDir := fs.String("out-dir", defaultWatchlistDir, "directory the dated watchlist file is written to (<out-dir>/YYYY-MM-DD.json)")
	dataDir := fs.String("data-dir", "data", "directory for the cached instrument master, constituent lists and daily candles")
	stateDir := fs.String("state-dir", "", "directory of the saved Kite session (default $XDG_STATE_HOME/jevtrader or ~/.local/state/jevtrader)")
	top := fs.Int("top", def.TopN, "number of instruments to put on the list")
	direction := fs.String("direction", def.Direction, `"long" ranks by expected rise; "both" ranks by the size of the score and also lists short candidates (needs --require-catalyst=false: only positive news is a catalyst)`)
	wNews := fs.Float64("w-news", def.Weights.News, "weight of the news signal (the primary signal)")
	wSector := fs.Float64("w-sector", def.Weights.Sector, "weight of the sector-strength signal (0: the study found no relation to returns)")
	wGap := fs.Float64("w-gap", def.Weights.Gap, "weight of the pre-open gap signal")
	wRange := fs.Float64("w-range", def.Weights.Range, "weight of the recent-range signal; 0 leaves it out, negative penalises a wide range (the study found wide-range stocks did worse)")
	minValueCr := fs.Float64("min-value-cr", def.Filters.MinPrevValue/1e7, "least previous-day traded value, in crore rupees")
	minADV := fs.Float64("min-adv", def.Filters.MinADV, "least average daily volume, in shares")
	minPrice := fs.Float64("min-price", def.Filters.MinPrice, "least previous close, in rupees")
	maxPrice := fs.Float64("max-price", def.Filters.MaxPrice, "greatest previous close, in rupees")
	maxPerSector := fs.Int("max-per-sector", def.MaxPerSector, "most entries from one sector; 0 for no cap")
	requireCatalyst := fs.Bool("require-catalyst", def.Catalyst.Require, "list only stocks with a news item Jev scored as material, positive and large enough (the list may then be short, or empty on a quiet day, and run then does not trade); --require-catalyst=false ranks every screened stock")
	catMaterial := fs.Float64("catalyst-material", def.Catalyst.MinMaterial, "least Jev 'material' score (0..1) of a catalyst item")
	catDirection := fs.Float64("catalyst-direction", def.Catalyst.MinDirection, "least Jev direction (P(up) - P(down), 0..1) of a catalyst item")
	catImpact := fs.Float64("catalyst-impact", def.Catalyst.MinImpact, "least Jev impact level (0 none, 1 under 1%, 2 about 1-2%, 3 about 2-4%, 4 over 4%) of a catalyst item")
	minScore := fs.Float64("min-score", def.MinScore, "least score an entry needs")
	newsWindow := fs.Duration("news-window", def.NewsWindow, "how far back to read news")
	maxNews := fs.Int("max-news", def.MaxNewsItems, "most (news item, company) pairs scored by Jev per run")
	sources := fs.String("news-sources", "all", `comma-separated news sources: nse-announcements and the RSS feeds `+feedNames()+`; "all" or "none"`)
	model := fs.String("model", jev.DefaultModel, "pinned Jev model id (\"jev-latest\" is rejected)")
	jevURL := fs.String("jev-url", "", "Jev base URL (default: the public API)")
	force := fs.Bool("force", false, "let an early screen replace a final watchlist already written for the day")
	fs.Usage = func() {
		fmt.Fprintln(e.stderr, "usage: jevtrader screen [flags]\n\nBuilds the day's watchlist before the session: NIFTY Midcap 150 and Smallcap 250\nstocks (less NIFTY 50 and Next 50) that are liquid enough for intraday, ranked on news\nscored by Jev (a material positive catalyst is required by default), the pre-open gap and,\nif weighted, sector strength and recent range. Writes\n<out-dir>/YYYY-MM-DD.json, which `jevtrader run --watchlist-file` reads.\nNeeds a Kite session (`jevtrader login`) for candles and quotes. Reads KITE_API_KEY and\nJEV_API_KEY from the environment, or from the Azure Key Vault named by JEVTRADER_KEYVAULT.\nWithout JEV_API_KEY the news is fetched but not scored.\n\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected arguments %q", fs.Args())}
	}

	now := time.Now()
	cfg := def
	var err error
	if cfg.Date, cfg.Phase, err = resolveScreenDay(*date, *phase, now); err != nil {
		return usageError{err}
	}
	cfg.Now = now
	cfg.TopN, cfg.Direction = *top, *direction
	cfg.Weights = screen.Weights{News: *wNews, Sector: *wSector, Gap: *wGap, Range: *wRange}
	cfg.Filters.MinPrevValue, cfg.Filters.MinADV = *minValueCr*1e7, *minADV
	cfg.Filters.MinPrice, cfg.Filters.MaxPrice = *minPrice, *maxPrice
	cfg.MaxPerSector, cfg.MinScore = *maxPerSector, *minScore
	cfg.Catalyst = screen.Catalyst{Require: *requireCatalyst, MinMaterial: *catMaterial, MinDirection: *catDirection, MinImpact: *catImpact}
	cfg.NewsWindow, cfg.MaxNewsItems = *newsWindow, *maxNews
	cfg.CacheDir = *dataDir
	if err := cfg.Validate(); err != nil {
		return usageError{err}
	}
	names, err := parseNewsSources(*sources)
	if err != nil {
		return usageError{err}
	}
	if *model == "jev-latest" {
		return usageError{errors.New(`--model "jev-latest" is rejected: pin an explicit model id`)}
	}

	path := watchlist.Path(*outDir, cfg.Date.Format(time.DateOnly))
	if cfg.Phase == watchlist.PhaseEarly && !*force {
		if existing, err := watchlist.Read(path); err == nil && existing.Final {
			fmt.Fprintf(e.stdout, "%s already holds the %s watchlist (phase %s); an early screen would replace it. Pass --force to do so.\n", path, existing.Date, existing.Phase)
			return nil
		}
	}

	logger := slog.New(slog.NewTextHandler(e.stderr, nil))
	deps, err := buildScreenDeps(ctx, e, screenOptions{
		dataDir: *dataDir, stateDir: *stateDir, model: *model, jevURL: *jevURL, sources: names,
		kiteURL: lookup(e, "KITE_API_BASE_URL"),
	}, logger)
	if err != nil {
		return err
	}
	file, err := screen.Run(ctx, cfg, deps)
	if err != nil {
		if errors.Is(err, screen.ErrAbort) {
			err = fmt.Errorf("%w (an expired Kite session or a missing historical-data permission: run `jevtrader login`, and check the Kite Connect plan)", err)
		}
		return err
	}
	if err := watchlist.Write(path, file); err != nil {
		return err
	}
	printWatchlist(e.stdout, path, file)
	return nil
}

func lookup(e env, key string) string { v, _ := e.lookup(key); return v }

// resolveScreenDay turns --date and --phase into the trading day (IST
// midnight) and the phase to run.
func resolveScreenDay(date, phase string, now time.Time) (time.Time, string, error) {
	nowIST := now.In(watchlist.IST)
	today := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 0, 0, 0, 0, watchlist.IST)
	day := today
	if date != "" {
		var err error
		if day, err = time.ParseInLocation(time.DateOnly, date, watchlist.IST); err != nil {
			return time.Time{}, "", fmt.Errorf("--date: %w", err)
		}
	}
	switch {
	case day.After(today):
		return time.Time{}, "", fmt.Errorf("--date %s is in the future", day.Format(time.DateOnly))
	case day.Before(today):
		if phase != "auto" && phase != watchlist.PhaseHistoric {
			return time.Time{}, "", fmt.Errorf("--phase %s needs today's pre-open; a past --date is screened in the historic phase", phase)
		}
		return day, watchlist.PhaseHistoric, nil
	}
	switch phase {
	case "auto":
		if nowIST.Sub(today) >= preOpenReadFrom {
			return day, watchlist.PhaseFinal, nil
		}
		return day, watchlist.PhaseEarly, nil
	case watchlist.PhaseEarly, watchlist.PhaseFinal:
		return day, phase, nil
	}
	return time.Time{}, "", fmt.Errorf("--phase %q is not auto, early or final", phase)
}

func feedNames() string {
	names := make([]string, len(rssFeeds))
	for i, f := range rssFeeds {
		names[i] = f.name
	}
	return strings.Join(names, ", ")
}

// parseNewsSources validates --news-sources and returns the source names.
func parseNewsSources(list string) ([]string, error) {
	known := map[string]bool{nseAnnouncementsName: true}
	all := []string{nseAnnouncementsName}
	for _, f := range rssFeeds {
		known[f.name] = true
		all = append(all, f.name)
	}
	switch strings.TrimSpace(list) {
	case "all", "":
		return all, nil
	case "none":
		return nil, nil
	}
	var out []string
	for _, n := range splitList(list) {
		if !known[n] {
			return nil, fmt.Errorf("--news-sources: unknown source %q (want one of %s, all or none)", n, strings.Join(all, ", "))
		}
		out = append(out, n)
	}
	return out, nil
}

// kiteConn is a Kite client and the session behind it.
type kiteConn struct {
	client                       *kiteconnect.Client
	apiKey, accessToken, baseURL string
}

// connectKite opens a Kite client from KITE_API_KEY (or the Key Vault) and the
// saved session or KITE_ACCESS_TOKEN.
func connectKite(ctx context.Context, e env, stateDir, baseURL string) (kiteConn, error) {
	secrets, err := loadSecrets(ctx, e, config.EnvKiteAPIKey)
	if err != nil {
		return kiteConn{}, usageError{err}
	}
	if secrets.KiteAPIKey.IsZero() {
		return kiteConn{}, usageError{errors.New("KITE_API_KEY is not set")}
	}
	apiKey := secrets.KiteAPIKey.Reveal()
	accessToken := lookup(e, "KITE_ACCESS_TOKEN")
	if accessToken == "" {
		accessToken = savedAccessToken(ctx, stateDir)
	}
	if accessToken == "" {
		return kiteConn{}, usageError{errors.New("no Kite session: run `jevtrader login` (or set KITE_ACCESS_TOKEN)")}
	}
	kc := kiteconnect.New(apiKey)
	kc.SetAccessToken(accessToken)
	if baseURL != "" {
		kc.SetBaseURI(baseURL)
	}
	return kiteConn{client: kc, apiKey: apiKey, accessToken: accessToken, baseURL: baseURL}, nil
}

// loadNSEMaster returns Kite's NSE instrument master, cached per day under dataDir.
func loadNSEMaster(ctx context.Context, c kiteConn, dataDir string) ([]domain.Instrument, error) {
	return kite.LoadMaster(ctx, dataDir, time.Now(), []string{"NSE"}, kite.HTTPMasterFetcher(nil, c.baseURL, c.apiKey, staticToken(c.accessToken)))
}

func realScreenDeps(ctx context.Context, e env, o screenOptions, logger *slog.Logger) (screen.Deps, error) {
	conn, err := connectKite(ctx, e, o.stateDir, o.kiteURL)
	if err != nil {
		return screen.Deps{}, err
	}
	master, err := loadNSEMaster(ctx, conn, o.dataDir)
	if err != nil {
		return screen.Deps{}, err
	}
	return screenDepsFor(ctx, e, o, conn, master, logger)
}

// screenDepsFor connects the screen to NSE, the news sources and Jev, and to
// Kite through conn, whose instrument master is master.
func screenDepsFor(ctx context.Context, e env, o screenOptions, conn kiteConn, master []domain.Instrument, logger *slog.Logger) (screen.Deps, error) {
	tokens := make(map[string]uint32, len(master))
	for _, in := range master {
		tokens[in.Exchange+":"+in.Symbol] = in.Token
	}

	d := screen.Deps{NSE: universe.HTTPFetcher(nil, "", ""), Market: screen.NewKite(conn.client, tokens), Model: o.model, Logger: logger}
	for _, name := range o.sources {
		if name == nseAnnouncementsName {
			d.News = append(d.News, news.NSEAnnouncements{})
			continue
		}
		for _, f := range rssFeeds {
			if f.name == name {
				d.News = append(d.News, news.RSS{FeedName: f.name, URL: f.url, Client: &http.Client{Timeout: 30 * time.Second}})
			}
		}
	}
	if len(d.News) > 0 {
		// Jev is optional: without it the news is fetched but not scored.
		js, err := loadSecrets(ctx, e, config.EnvJevAPIKey)
		switch {
		case err != nil || js.JevAPIKey.IsZero():
			reason := "JEV_API_KEY is not set"
			if err != nil {
				reason = err.Error()
			}
			fmt.Fprintf(e.stderr, "jevtrader screen: no Jev API key (%s); the news will not be scored\n", reason)
		default:
			jc, err := jev.New(jev.Config{APIKey: js.JevAPIKey.Reveal(), Model: o.model, BaseURL: o.jevURL, MaxBackoff: 5 * time.Second, Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				return screen.Deps{}, usageError{err}
			}
			d.Jev = jc
		}
	}
	return d, nil
}

// printWatchlist writes a human summary of the file.
func printWatchlist(w io.Writer, path string, f watchlist.File) {
	fmt.Fprintf(w, "Watchlist for %s (%s phase, %d instruments) written to %s\n\n", f.Date, f.Phase, len(f.Entries), path)
	if len(f.Entries) == 0 {
		fmt.Fprintln(w, "No catalysts today: no stock has a material positive news item, so the list is empty and `run --watchlist-file` will not trade.")
	}
	for _, e := range f.Entries {
		fmt.Fprintf(w, "%2d. %-16s %+.2f  %s (%s)\n", e.Rank, e.Key, e.Score, e.Name, e.Sector)
		for _, r := range e.Reasons {
			fmt.Fprintf(w, "      - %s\n", r)
		}
	}
	fmt.Fprintln(w, "\nSectors:")
	for _, s := range f.Sectors {
		fmt.Fprintf(w, "  %-36s %+.2f  (%d liquid stocks, median previous session %+.1f%%)\n", s.Name, s.Score, s.Members, s.PrevReturn)
	}
	fmt.Fprintln(w, "\nSources:")
	for _, s := range f.Sources {
		line := fmt.Sprintf("  %-30s %-8s %d", s.Name, s.Status, s.Items)
		if s.Detail != "" {
			line += "  " + s.Detail
		}
		fmt.Fprintln(w, line)
	}
	for _, n := range f.Notes {
		fmt.Fprintf(w, "\nnote: %s\n", n)
	}
}
