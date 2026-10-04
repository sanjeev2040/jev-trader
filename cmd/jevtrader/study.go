package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/sanjeev2040/jev-trader/internal/bars"
	"github.com/sanjeev2040/jev-trader/internal/config"
	"github.com/sanjeev2040/jev-trader/internal/decisionlog"
	"github.com/sanjeev2040/jev-trader/internal/domain"
	"github.com/sanjeev2040/jev-trader/internal/engine"
	"github.com/sanjeev2040/jev-trader/internal/jev"
	"github.com/sanjeev2040/jev-trader/internal/marketdata"
	mdreplay "github.com/sanjeev2040/jev-trader/internal/marketdata/replay"
	"github.com/sanjeev2040/jev-trader/internal/nsecal"
	"github.com/sanjeev2040/jev-trader/internal/orders/paper"
	"github.com/sanjeev2040/jev-trader/internal/replay"
	"github.com/sanjeev2040/jev-trader/internal/screen"
	"github.com/sanjeev2040/jev-trader/internal/snapshot"
	"github.com/sanjeev2040/jev-trader/internal/store"
	"github.com/sanjeev2040/jev-trader/internal/study"
	"github.com/sanjeev2040/jev-trader/internal/watchlist"
)

const (
	defaultHolidayFile = "deploy/nse-holidays.yaml"
	// studyIndexKey is the index every snapshot carries as context.
	studyIndexKey = "NSE:NIFTY 50"
	// studyFetchBatch is how many instruments one backfill step fetches before
	// writing them and noting them in the day's manifest.
	studyFetchBatch = 25
)

func runStudy(ctx context.Context, args []string, e env) int {
	if err := studyCmd(ctx, args, e); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(e.stderr, "jevtrader study: %v\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

// studyOptions are everything the real study connections need beyond the
// study's own config.
type studyOptions struct {
	dataDir, studyDir, stateDir, kiteURL, jevURL, model string
	first, last                                         time.Time
	screen                                              screen.Config
	sources                                             []string
	window                                              engine.Window
	capital                                             config.Capital
	rules                                               *tradingRules
	deciders                                            []string // every decider the study replays, for the secrets it needs
	latency, decideTimeout                              time.Duration
	horizon                                             int
	lookahead                                           []time.Duration
	logPath                                             string
	// jevRate is the most Jev calls a second (news scoring and decisions);
	// 0 is no limit.
	jevRate float64
}

// buildStudyDeps connects the study to Kite, the news sources and Jev. Tests
// replace it with fixtures. The returned function releases what it opened.
var buildStudyDeps = realStudyDeps

func studyCmd(ctx context.Context, args []string, e env) error {
	fs := flag.NewFlagSet("study", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	def := screen.DefaultConfig()
	from := fs.String("from", "", "first IST day to study, YYYY-MM-DD (or give --weeks)")
	to := fs.String("to", "", "last IST day to study, YYYY-MM-DD (default: yesterday)")
	weeks := fs.Int("weeks", 0, "study this many weeks ending at --to instead of giving --from")
	dataDir := fs.String("data-dir", "data", "directory for the cached instrument master, constituent lists, daily candles and news scores; the study keeps its own files under <data-dir>/study")
	stateDir := fs.String("state-dir", "", "directory of the saved Kite session (default $XDG_STATE_HOME/jevtrader or ~/.local/state/jevtrader)")
	out := fs.String("out", "", `report file (Markdown), with the same name ending in .json beside it (default <data-dir>/study/report-<from>_<to>.md); "-" writes the Markdown to stdout`)
	calendar := fs.String("calendar", defaultHolidayFile, `NSE holiday calendar; "" studies every weekday and skips a day Kite has no candles for`)
	start := fs.String("start", "09:15", "first IST time of day to trade, HH:MM")
	end := fs.String("end", "10:15", "IST time of day to stop at, HH:MM; the comparison and the buy-and-hold end here")
	deciders := fs.String("deciders", "jev,hold,momentum", "comma-separated deciders replayed over the picks: hold, momentum, jev")
	randomDeciders := fs.String("random-deciders", "momentum", `comma-separated deciders replayed over the random list (the buy-and-hold of the random list is always read); "" for none. jev here doubles the token cost`)
	randomSize := fs.Int("random-size", 0, "stocks in the random list; 0 is as many as the day's picks")
	noRandom := fs.Bool("no-random", false, "leave out the random list, and its backfill")
	seed := fs.Int64("seed", 1, "seed of the random lists; the same seed, day and screened pool give the same list")
	capitalFlag := fs.Float64("capital", 0, capitalFlagHelp)
	rules := addTradingRuleFlags(fs)
	latency := fs.Duration("latency", time.Second, "decision latency applied to every answer, so a result does not depend on how fast Jev answered")
	decideTimeout := fs.Duration("decide-timeout", 20*time.Second, "per-call decider deadline; a call that misses it becomes a HOLD, which the report counts")
	horizon := fs.Int("horizon", replay.DefaultHorizon, "bars ahead BUY and SELL calls are scored for calibration")
	lookahead := fs.String("lookahead", "5m,15m,30m", "how long after each BUY/SELL to check the price, comma-separated")
	refresh := fs.Bool("refresh", false, "recompute stored watchlists and day results (stored candles and Jev answers are still reused)")
	reportOnly := fs.Bool("report-only", false, "call nothing: build the report from the day results stored by an earlier run with the same settings")
	logPath := fs.String("log", "", "decision log (SQLite) the replays are written to (default <data-dir>/study/decisions.db)")
	top := fs.Int("top", def.TopN, "number of instruments the picker puts on the list")
	wNews := fs.Float64("w-news", def.Weights.News, "weight of the news signal (the primary signal)")
	wSector := fs.Float64("w-sector", def.Weights.Sector, "weight of the sector-strength signal (0: the study found no relation to returns)")
	wGap := fs.Float64("w-gap", def.Weights.Gap, "weight of the pre-open gap signal (a past day has none; it is left out of every score)")
	wRange := fs.Float64("w-range", def.Weights.Range, "weight of the recent-range signal; 0 leaves it out, negative penalises a wide range (the study found wide-range stocks did worse)")
	maxPerSector := fs.Int("max-per-sector", def.MaxPerSector, "most picks from one sector; 0 for no cap")
	requireCatalyst := fs.Bool("require-catalyst", def.Catalyst.Require, "list only stocks with a news item Jev scored as material, positive and large enough (the list may then be short, or empty on a quiet day, and run then does not trade); --require-catalyst=false ranks every screened stock")
	catMaterial := fs.Float64("catalyst-material", def.Catalyst.MinMaterial, "least Jev 'material' score (0..1) of a catalyst item")
	catDirection := fs.Float64("catalyst-direction", def.Catalyst.MinDirection, "least Jev direction (P(up) - P(down), 0..1) of a catalyst item")
	catImpact := fs.Float64("catalyst-impact", def.Catalyst.MinImpact, "least Jev impact level (0 none, 1 under 1%, 2 about 1-2%, 3 about 2-4%, 4 over 4%) of a catalyst item")
	minScore := fs.Float64("min-score", def.MinScore, "least score a pick needs")
	maxNews := fs.Int("max-news", def.MaxNewsItems, "most (news item, company) pairs Jev scores per day; raise it (600 or more) with --universe, where every screened stock's news counts")
	anonymize := fs.Bool("anonymize-news", false, "look-ahead control: hide the company's name, its symbol and every date from the text sent to Jev, so the news scores come from the wording alone. Run the same range with and without it and compare the news tables; the scores are cached apart")
	universe := fs.Bool("universe", false, "measure every stock of the screened pool each day (hold-only: its return over the window and to the close), so the market-neutral benchmark is the whole pool and the news table covers every stock with a scored item, negative news included. Fetches the 1-minute candles of the whole pool (about 390 stocks a day)")
	jevRate := fs.Float64("jev-rate", 10, "most Jev calls a second, for the news scoring and the decisions that are not answered from the store; 0 for no limit")
	sources := fs.String("news-sources", "all", `news sources, as for screen; RSS feeds keep no history, so a past day reads NSE filings only`)
	model := fs.String("model", jev.DefaultModel, "pinned Jev model id (\"jev-latest\" is rejected)")
	jevURL := fs.String("jev-url", "", "Jev base URL (default: the public API)")
	fs.Usage = func() {
		fmt.Fprintln(e.stderr, `usage: jevtrader study (--from YYYY-MM-DD | --weeks N) [--to YYYY-MM-DD] [flags]

Backtests the pre-market picker over many trading days. For each day it screens as
of that morning (NSE filings and daily candles public before the open), backfills
the picks and a random list of the same size drawn from the same screened pool,
replays a window with the deciders, and aggregates: net P&L per day and in total
against buy-and-hold of the picks and of the random list, hit rates, calibration,
which signals went with the window return, and what Jev cost in tokens.

Everything is cached under --data-dir (daily candles, minute candles, news scores,
Jev answers, day results), so a rerun or a resumed run asks Kite and Jev only for
what is new. It needs a Kite session with the historical-data plan (jevtrader
login) and reads KITE_API_KEY and JEV_API_KEY from the environment or from the
Azure Key Vault named by JEVTRADER_KEYVAULT. See docs/picker-study.md.

flags:`)
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

	first, last, err := studyRange(*from, *to, *weeks, time.Now())
	if err != nil {
		return usageError{err}
	}
	if *model == "jev-latest" {
		return usageError{errors.New(`--model "jev-latest" is rejected: pin an explicit model id`)}
	}
	names := splitList(*deciders)
	if len(names) == 0 {
		return usageError{errors.New("--deciders is empty")}
	}
	rnames := splitList(*randomDeciders)
	for _, name := range append(slices.Clone(names), rnames...) {
		if name != "hold" && name != "momentum" && name != "jev" {
			return usageError{fmt.Errorf("decider %q is not hold, momentum or jev", name)}
		}
	}
	window, err := parseWindow(*start, *end)
	if err != nil {
		return usageError{err}
	}
	horizons, err := parseLookahead(*lookahead)
	if err != nil {
		return usageError{err}
	}
	srcNames, err := parseNewsSources(*sources)
	if err != nil {
		return usageError{err}
	}
	if *noRandom && *randomSize != 0 {
		return usageError{errors.New("--no-random and --random-size both choose the random list; give one")}
	}
	if *randomSize < 0 {
		return usageError{errors.New("--random-size must not be negative")}
	}
	if *jevRate < 0 {
		return usageError{errors.New("--jev-rate must not be negative")}
	}

	cfg := def
	cfg.Phase = watchlist.PhaseHistoric
	cfg.TopN, cfg.MaxPerSector, cfg.MinScore = *top, *maxPerSector, *minScore
	cfg.MaxNewsItems, cfg.AnonymizeNews = *maxNews, *anonymize
	cfg.Weights = screen.Weights{News: *wNews, Sector: *wSector, Gap: *wGap, Range: *wRange}
	cfg.Catalyst = screen.Catalyst{Require: *requireCatalyst, MinMaterial: *catMaterial, MinDirection: *catDirection, MinImpact: *catImpact}
	cfg.CacheDir, cfg.KeepPool = *dataDir, true
	cfg.Date = first
	if err := cfg.Validate(); err != nil {
		return usageError{err}
	}

	cal, err := loadCalendar(*calendar, e.stderr)
	if err != nil {
		return err
	}
	studyDir := filepath.Join(*dataDir, "study")
	if *logPath == "" {
		*logPath = filepath.Join(studyDir, "decisions.db")
	}
	if err := rules.validate(); err != nil {
		return usageError{err}
	}
	capital, err := resolveCapital(ctx, e, *capitalFlag)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stderr, "jevtrader study: capital budget ₹%.0f (%s: %s)\n", capital.Amount, capital.Source, capital.Detail)

	randomN := *randomSize
	if *noRandom {
		randomN = -1
		rnames = nil
	}
	screenKey := fingerprint("screen/v3", cfg.TopN, cfg.MaxPerSector, cfg.MinScore, cfg.Weights, cfg.Catalyst, cfg.Filters, cfg.NewsWindow, cfg.MaxNewsItems, cfg.AnonymizeNews, srcNames, *model)
	runKey := fingerprint("run/v3", screenKey, *start, *end, names, rnames, randomN, *seed, *universe, capital.Amount, rules.String(), *latency, *horizon, horizons, *model)

	sc := study.Config{
		From: first.Format(time.DateOnly), To: last.Format(time.DateOnly), Dir: studyDir, Calendar: cal,
		RandomN: randomN, Seed: *seed, PickDeciders: names, RandomDeciders: rnames,
		ScreenKey: screenKey, RunKey: runKey, Refresh: *refresh, CachedOnly: *reportOnly, Universe: *universe,
		Logger: slog.New(slog.NewTextHandler(e.stderr, nil)),
	}
	var deps study.Deps
	if !*reportOnly {
		d, closeDeps, err := buildStudyDeps(ctx, e, studyOptions{
			dataDir: *dataDir, studyDir: studyDir, stateDir: *stateDir, kiteURL: lookup(e, "KITE_API_BASE_URL"),
			jevURL: *jevURL, model: *model, first: first, last: last, screen: cfg, sources: srcNames,
			window: window, capital: capital, rules: rules, deciders: append(slices.Clone(names), rnames...), latency: *latency, decideTimeout: *decideTimeout,
			horizon: *horizon, lookahead: horizons, logPath: *logPath, jevRate: *jevRate,
		}, sc.Logger)
		if err != nil {
			return err
		}
		defer closeDeps()
		deps = d
	}

	results, runErr := study.Run(ctx, sc, deps)
	if runErr != nil && len(results) == 0 {
		return runErr
	}
	if runErr != nil {
		fmt.Fprintf(e.stderr, "jevtrader study: stopped: %v\nThe %d day(s) done are stored; run again to resume, or add --report-only for a report of them.\n", runErr, len(results))
	}
	summary := study.Summarize(sc.From, sc.To, results)
	meta := study.Meta{
		Generated: time.Now(), Horizon: *horizon, CostPct: roundTripCostPct(buyHoldNotional(tradingLimits(capital, rules)), paper.DefaultConfig()),
		Settings: studySettings(sc, cfg, srcNames, *start, *end, *model, capital, rules, *latency, horizons),
	}
	if err := writeStudyReport(e, *out, studyDir, summary, meta); err != nil {
		return err
	}
	return runErr
}

// studyRange resolves --from, --to and --weeks into the first and last day.
func studyRange(from, to string, weeks int, now time.Time) (first, last time.Time, err error) {
	nowIST := now.In(watchlist.IST)
	today := time.Date(nowIST.Year(), nowIST.Month(), nowIST.Day(), 0, 0, 0, 0, watchlist.IST)
	last = today.AddDate(0, 0, -1)
	if to != "" {
		if last, err = time.ParseInLocation(time.DateOnly, to, watchlist.IST); err != nil {
			return first, last, fmt.Errorf("--to: %w", err)
		}
	}
	switch {
	case !last.Before(today):
		return first, last, fmt.Errorf("--to %s is not in the past: a study replays finished sessions", last.Format(time.DateOnly))
	case from != "" && weeks != 0:
		return first, last, errors.New("--from and --weeks both set the start; give one")
	case from == "" && weeks <= 0:
		return first, last, errors.New("--from or --weeks is required")
	case weeks < 0:
		return first, last, errors.New("--weeks must be positive")
	case weeks > 0:
		first = last.AddDate(0, 0, -7*weeks+1)
	default:
		if first, err = time.ParseInLocation(time.DateOnly, from, watchlist.IST); err != nil {
			return first, last, fmt.Errorf("--from: %w", err)
		}
	}
	if last.Before(first) {
		return first, last, errors.New("--to is before --from")
	}
	return first, last, nil
}

func loadCalendar(path string, warn io.Writer) (*nsecal.Calendar, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && path == defaultHolidayFile {
		// Run from outside the repository: the study can still tell weekdays
		// from weekends, and Kite tells a holiday by having no candles.
		fmt.Fprintf(warn, "jevtrader study: %s not found; studying every weekday and skipping the days Kite has no candles for (--calendar names another file)\n", path)
		return nil, nil
	}
	if err != nil {
		return nil, usageError{fmt.Errorf("--calendar: %w", err)}
	}
	defer f.Close()
	cal, err := nsecal.Parse(f)
	if err != nil {
		return nil, usageError{fmt.Errorf("--calendar %s: %w", path, err)}
	}
	return cal, nil
}

// fingerprint is a short hash of what a stored result depends on.
func fingerprint(parts ...any) string {
	body, _ := json.Marshal(parts)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:6])
}

func studySettings(sc study.Config, cfg screen.Config, sources []string, start, end, model string, capital config.Capital, rules *tradingRules, latency time.Duration, lookahead []time.Duration) [][2]string {
	random := fmt.Sprintf("%d stocks of the screened pool, seed %d, replayed with %s", sc.RandomN, sc.Seed, strings.Join(sc.RandomDeciders, ", "))
	switch {
	case sc.RandomN < 0:
		random = "none"
	case sc.RandomN == 0:
		random = fmt.Sprintf("as many stocks as the day's picks, drawn from the screened pool, seed %d, replayed with %s", sc.Seed, strings.Join(sc.RandomDeciders, ", "))
	}
	w, k := cfg.Weights, cfg.Catalyst
	catalyst := "not required"
	if k.Require {
		catalyst = fmt.Sprintf("required (material ≥ %.2f, direction ≥ %.2f, impact ≥ %.1f)", k.MinMaterial, k.MinDirection, k.MinImpact)
	}
	return [][2]string{
		{"Days", sc.From + " to " + sc.To},
		{"Window", fmt.Sprintf("%s–%s IST", start, end)},
		{"Picker", fmt.Sprintf("top %d, at most %d a sector, minimum score %.2f, weights news %.2f sector %.2f gap %.2f range %.2f (the gap is absent on past days), news catalyst %s, news from %s", cfg.TopN, cfg.MaxPerSector, cfg.MinScore, w.News, w.Sector, w.Gap, w.Range, catalyst, strings.Join(sources, ", "))},
		{"News scoring", newsScoring(cfg)},
		{"Stocks measured", measured(sc)},
		{"Deciders on the picks", strings.Join(sc.PickDeciders, ", ")},
		{"Random list", random},
		{"Jev model", model},
		{"Capital", fmt.Sprintf("₹%.0f (%s)", capital.Amount, capital.Source)},
		{"Trading rules", rules.String()},
		{"Decision latency", latency.String()},
		{"Candles", "Kite 1m historical candles (not tick-built bars)"},
		{"Keys", "screen " + sc.ScreenKey + ", run " + sc.RunKey},
	}
}

// roundTripCostPct is what buying and selling notional rupees of a stock costs
// as a percent of the entry: the charges of both orders and the slippage of
// each side, at a typical price.
func roundTripCostPct(notional float64, p paper.Config) float64 {
	const price = 100.0
	qty := int64(notional / price)
	if qty <= 0 {
		return 0
	}
	charges := p.Costs.Charges(domain.ActionBuy, qty, price) + p.Costs.Charges(domain.ActionSell, qty, price)
	return charges/(float64(qty)*price)*100 + 2*p.SlippageBps/100
}

func newsScoring(cfg screen.Config) string {
	if cfg.AnonymizeNews {
		return fmt.Sprintf("anonymised: the company's name and symbol and every date hidden from Jev (the look-ahead control); at most %d pairs a day", cfg.MaxNewsItems)
	}
	return fmt.Sprintf("named: the company and the publication time are sent to Jev; at most %d pairs a day", cfg.MaxNewsItems)
}

func measured(sc study.Config) string {
	if sc.Universe {
		return "the picks, the random list and every other stock of the screened pool (hold only), so the market-neutral benchmark is the whole pool"
	}
	return "the picks and the random list only; the market-neutral benchmark is the random list (--universe measures the whole pool)"
}

func writeStudyReport(e env, out, studyDir string, s study.Summary, meta study.Meta) error {
	if out == "-" {
		return study.WriteMarkdown(e.stdout, s, meta)
	}
	if out == "" {
		out = filepath.Join(studyDir, fmt.Sprintf("report-%s_%s.md", s.From, s.To))
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	md, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := study.WriteMarkdown(md, s, meta); err != nil {
		md.Close()
		return err
	}
	if err := md.Close(); err != nil {
		return err
	}
	jsonPath := strings.TrimSuffix(out, filepath.Ext(out)) + ".json"
	js, err := os.Create(jsonPath)
	if err != nil {
		return err
	}
	if err := study.WriteJSON(js, s); err != nil {
		js.Close()
		return err
	}
	if err := js.Close(); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Studied %d trading day(s), %d skipped.\nReport: %s\nData:   %s\n", s.OKDays, len(s.Skipped), out, jsonPath)
	return nil
}

// realStudyDeps connects the study to the real systems.
func realStudyDeps(ctx context.Context, e env, o studyOptions, logger *slog.Logger) (study.Deps, func(), error) {
	noop := func() {}
	conn, err := connectKite(ctx, e, o.stateDir, o.kiteURL)
	if err != nil {
		return study.Deps{}, noop, err
	}
	master, err := loadNSEMaster(ctx, conn, o.dataDir)
	if err != nil {
		return study.Deps{}, noop, err
	}
	sd, err := screenDepsFor(ctx, e, screenOptions{dataDir: o.dataDir, stateDir: o.stateDir, model: o.model, jevURL: o.jevURL, kiteURL: o.kiteURL, sources: o.sources}, conn, master, logger)
	if err != nil {
		return study.Deps{}, noop, err
	}
	sd.Market = study.NewRangeMarket(sd.Market, o.first, o.last, screen.DailyLookbackDays)
	if sd.Jev != nil {
		sd.Jev = screen.NewLimitedAsker(sd.Jev, o.jevRate)
	}

	var wanted []string
	if slices.Contains(o.deciders, "jev") {
		wanted = []string{config.EnvJevAPIKey}
	}
	secrets, err := loadSecrets(ctx, e, wanted...)
	if err != nil {
		return study.Deps{}, noop, usageError{err}
	}
	if err := os.MkdirAll(o.studyDir, 0o755); err != nil {
		return study.Deps{}, noop, err
	}
	log, err := decisionlog.Open(o.logPath, decisionlog.Options{Synchronous: "NORMAL"})
	if err != nil {
		return study.Deps{}, noop, err
	}
	answers, err := study.OpenAnswerStore(filepath.Join(o.studyDir, "jev-answers.jsonl"))
	if err != nil {
		log.Close()
		return study.Deps{}, noop, err
	}
	release := func() { answers.Close(); log.Close() }

	byKey := make(map[string]domain.Instrument, len(master))
	for _, in := range master {
		byKey[in.Exchange+":"+in.Symbol] = in
	}
	index, ok := byKey[studyIndexKey]
	if !ok {
		release()
		return study.Deps{}, noop, fmt.Errorf("%s is not in the instrument master", studyIndexKey)
	}
	bf := &studyBackfiller{
		root: o.studyDir, kite: conn.client, limiter: rate.NewLimiter(rate.Every(time.Second/historicalRate), 1),
		byKey: byKey, index: index, log: logger,
	}
	rp := &studyReplayer{o: o, root: o.studyDir, byKey: byKey, index: index, log: log, answers: answers, secrets: secrets}
	if o.jevRate > 0 {
		rp.limiter = rate.NewLimiter(rate.Limit(o.jevRate), 1)
	}
	deps := study.Deps{
		Screen: func(ctx context.Context, day string) (watchlist.File, error) {
			cfg := o.screen
			cfg.Date, _ = time.ParseInLocation(time.DateOnly, day, watchlist.IST)
			cfg.Now = time.Now()
			return screen.Run(ctx, cfg, sd)
		},
		Backfill: bf.fetch,
		Replay:   rp.replay,
		Returns:  rp.returns,
	}
	return deps, release, nil
}

// studyBackfiller keeps each studied day's 1m candles under <root>/bars_backfill,
// the same store `backfill` writes, apart from the data dir's own.
type studyBackfiller struct {
	root    string
	kite    historian
	limiter *rate.Limiter
	byKey   map[string]domain.Instrument
	index   domain.Instrument
	log     *slog.Logger
}

// manifest says which instruments of a day were fetched and how many 1m
// candles each had, so a rerun fetches only what is new. An empty answer is not
// recorded, so a rerun asks again in case the cause was temporary.
type manifest map[string]int

func (b *studyBackfiller) manifestPath(day string) string {
	return filepath.Join(b.root, "fetched", day+".json")
}

func (b *studyBackfiller) readManifest(day string) manifest {
	m := manifest{}
	if body, err := os.ReadFile(b.manifestPath(day)); err == nil {
		_ = json.Unmarshal(body, &m) // a damaged manifest only costs a refetch
	}
	if m == nil {
		m = manifest{}
	}
	return m
}

func (b *studyBackfiller) writeManifest(day string, m manifest) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.manifestPath(day)), 0o755); err != nil {
		return err
	}
	tmp := b.manifestPath(day) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.manifestPath(day))
}

func (b *studyBackfiller) fetch(ctx context.Context, day string, keys []string) ([]string, error) {
	date, err := time.ParseInLocation(time.DateOnly, day, ist)
	if err != nil {
		return nil, err
	}
	m := b.readManifest(day)
	want := append([]string{studyIndexKey}, keys...)
	var todo []domain.Instrument
	var missing []string
	for _, k := range want {
		in, ok := b.byKey[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case !slices.ContainsFunc(todo, func(t domain.Instrument) bool { return t.Token == in.Token }):
			if _, done := m[k]; !done {
				todo = append(todo, in)
			}
		}
	}
	for start := 0; start < len(todo); start += studyFetchBatch {
		batch := todo[start:min(start+studyFetchBatch, len(todo))]
		job := backfillJob{from: date, to: date, dataDir: b.root, instruments: map[uint32]domain.Instrument{}}
		for _, in := range batch {
			job.tokens = append(job.tokens, in.Token)
			job.instruments[in.Token] = in
		}
		got, err := job.fetchChunk(ctx, b.kite, b.limiter, date, date.Add(24*time.Hour-time.Second))
		if err != nil {
			return nil, err
		}
		w, err := store.NewBarWriter(b.root, store.KindBackfill)
		if err != nil {
			return nil, err
		}
		if err := errors.Join(w.Write(got...), w.Close()); err != nil {
			return nil, err
		}
		counts := map[uint32]int{}
		for _, bar := range got {
			counts[bar.Instrument.Token]++
		}
		for _, in := range batch {
			if n := counts[in.Token]; n > 0 {
				m[in.Exchange+":"+in.Symbol] = n
			}
		}
		if err := b.writeManifest(day, m); err != nil {
			return nil, err
		}
		b.log.Info("backfilled", "day", day, "instruments", len(batch), "bars", len(got))
	}
	if m[studyIndexKey] == 0 {
		return nil, study.ErrNoData
	}
	for _, k := range keys {
		if _, ok := b.byKey[k]; ok && m[k] == 0 {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing), nil
}

// studyReplayer replays a studied day.
type studyReplayer struct {
	o       studyOptions
	root    string
	byKey   map[string]domain.Instrument
	index   domain.Instrument
	log     *decisionlog.Log
	answers *study.AnswerStore
	secrets config.Secrets
	// limiter spaces the Jev calls that are paid for, across every replay.
	limiter *rate.Limiter
}

// sessionOpen is the NSE session open, as an offset from IST midnight; the
// window never opens before it.
const sessionOpen = 9*time.Hour + 15*time.Minute

// returns reads each key's return over the study window of the day from the
// stored 1-minute candles, as the replay's buy-and-hold reads it (the open of
// the window's first bar to the close of its last, and to the day's last
// close), without running the engine. A key with no bar in the window is left
// out.
func (r *studyReplayer) returns(_ context.Context, day string, keys []string) (map[string]study.Return, error) {
	date, err := time.ParseInLocation(time.DateOnly, day, ist)
	if err != nil {
		return nil, err
	}
	byToken := make(map[uint32]string, len(keys))
	tokens := make([]uint32, 0, len(keys))
	for _, k := range keys {
		in, ok := r.byKey[k]
		if !ok {
			return nil, fmt.Errorf("%s is not in the instrument master", k)
		}
		byToken[in.Token] = k
		tokens = append(tokens, in.Token)
	}
	start := date.Add(max(r.o.window.Start, sessionOpen))
	end := date.AddDate(0, 0, 1)
	if r.o.window.End != 0 {
		end = date.Add(r.o.window.End)
	}
	type span struct {
		first, last *domain.Bar
		dayClose    float64
	}
	spans := make(map[uint32]*span, len(keys))
	err = store.ScanBars(r.root, store.KindBackfill, store.BarQuery{Query: store.Query{From: date, To: date.AddDate(0, 0, 1), Tokens: tokens}, Interval: time.Minute}, func(b domain.Bar) error {
		sp := spans[b.Instrument.Token]
		if sp == nil {
			sp = &span{}
			spans[b.Instrument.Token] = sp
		}
		sp.dayClose = b.Close
		if !b.Start.Before(start) && !b.End.After(end) {
			if sp.first == nil {
				bar := b
				sp.first = &bar
			}
			bar := b
			sp.last = &bar
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]study.Return, len(spans))
	for token, sp := range spans {
		if sp.first == nil || sp.first.Open <= 0 {
			continue
		}
		open := sp.first.Open
		ret := study.Return{WindowPct: (sp.last.Close - open) / open * 100}
		if sp.dayClose > 0 {
			v := (sp.dayClose - open) / open * 100
			ret.DayPct = &v
		}
		out[byToken[token]] = ret
	}
	return out, nil
}

func (r *studyReplayer) replay(ctx context.Context, req study.ReplayRequest) (*replay.Report, study.Usage, error) {
	day, err := time.ParseInLocation(time.DateOnly, req.Day, ist)
	if err != nil {
		return nil, study.Usage{}, err
	}
	src := mdreplay.Config{Root: r.root, Kind: mdreplay.Backfill, From: day, To: day.AddDate(0, 0, 1), Tokens: []uint32{r.index.Token}}
	for _, k := range req.Keys {
		in, ok := r.byKey[k]
		if !ok {
			return nil, study.Usage{}, fmt.Errorf("%s is not in the instrument master", k)
		}
		src.Tokens = append(src.Tokens, in.Token)
	}
	// The Jev client is built here, not by buildSpecs, to keep its per-call log
	// line out of a study of tens of thousands of calls.
	var specs []replay.Spec
	var cached *study.CachedDecider
	for _, name := range req.Deciders {
		if name != "jev" {
			one, err := buildSpecs(ctx, []string{name}, r.log, r.secrets, r.o.model, r.o.jevURL)
			if err != nil {
				return nil, study.Usage{}, err
			}
			specs = append(specs, one...)
			continue
		}
		if err := r.secrets.RequireJev(); err != nil {
			return nil, study.Usage{}, usageError{err}
		}
		client, err := jev.New(jev.Config{APIKey: r.secrets.JevAPIKey.Reveal(), Model: r.o.model, BaseURL: r.o.jevURL, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			return nil, study.Usage{}, err
		}
		cached = &study.CachedDecider{Inner: client, Model: r.o.model, Store: r.answers, Limiter: r.limiter}
		specs = append(specs, replay.Spec{Name: "jev", Decider: cached})
	}
	limits := tradingLimits(r.o.capital, r.o.rules)
	rep, err := replay.Run(ctx, replay.Config{
		Log: r.log,
		NewSource: func() (marketdata.Source, error) {
			return mdreplay.New(src)
		},
		SourceName: string(src.Kind), Label: src.Label(),
		Limits: limits, Paper: paper.DefaultConfig(), Bars: bars.Config{}, DecideEvery: r.o.rules.decideEvery, Rules: r.o.rules.String(),
		Snapshot:   snapshot.Config{SizeThresholds: sizeThresholds(limits)},
		IndexToken: r.index.Token, RunPrefix: fmt.Sprintf("study-%s-%s-%s", req.Day, req.List, time.Now().UTC().Format("150405.000000")),
		Window: r.o.window, Latency: r.o.latency, DecideTimeout: r.o.decideTimeout, Horizon: r.o.horizon,
		Compare: true, Lookahead: r.o.lookahead, BuyHoldNotional: buyHoldNotional(limits),
	}, specs)
	var usage study.Usage
	if cached != nil {
		usage = cached.TakeUsage()
	}
	return rep, usage, err
}
