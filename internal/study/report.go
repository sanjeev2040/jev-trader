package study

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/screen"
)

// Meta describes how a study was run, for the report's header.
type Meta struct {
	// Settings are label and value lines: window, capital, model, weights, ...
	Settings [][2]string
	// Generated is when the report was written.
	Generated time.Time
	// Horizon is the bars ahead a BUY or SELL is scored for calibration.
	Horizon int
	// CostPct is the round-trip cost of a long position, in percent of the
	// entry (charges and slippage), for the market-neutral tables' "after
	// costs" column; zero leaves the column out.
	CostPct float64
}

// Limitations are the things about a past-day study that make it differ from
// what the picker does live, printed with every report.
var Limitations = []string{
	"**Index constituents are today's.** NSE publishes the Midcap 150 and Smallcap 250 lists as they are now. A stock that has since left, or joined, is treated as in or out on every past day; delisted and renamed stocks are missing from Kite's instrument master and cannot be studied at all (survivorship). Their ASM/GSM status is today's too.",
	"**There is no pre-open gap on a past day.** The call auction left no quote to read, so the gap signal is absent and its weight is spread over the rest (reports/0004-watchlist.md). The sectoral-index move is absent for the same reason: a sector's strength rests on the previous session's returns alone. The live picker's final pass sees more than the picker studied here.",
	"**News is NSE filings only.** The RSS feeds keep no history. The live picker also reads five business-news feeds, so the news signal here is thinner than live.",
	"**The model may have seen these days.** Jev's training cutoff is unknown, so a replay over a past period is an optimistic upper bound for Jev, not evidence of an edge. Forward paper trading is the deciding evidence. The news scores are exposed to it too (the request names the company and the date): run with `--anonymize-news` and compare the news tables to see how much of the news signal survives without them.",
	"**Minute candles, not the order book.** Orders fill on minute candles after the decision latency, with the paper broker's costs; real fills in small and mid caps, which are thin in the first minutes after the open, will be worse.",
	"**Few days, correlated stocks.** The stocks of one day share the market's move, so the effective sample is closer to the number of days than to the number of stock-days, and the t statistics (which treat days as independent) flatter. The market-neutral intervals resample days, so they do not; read a difference as evidence only when its interval excludes zero and it has the same sign in most weeks.",
	"**Buy-and-hold deploys a fixed amount per name** (the size-M notional, a tenth of the budget) in every name of the list, which is far more than the budget the deciders trade with. Compare lists by mean return where the amounts differ; compare deciders with buy-and-hold by net P&L only as the same-sized-position benchmark the replay command uses.",
}

// WriteJSON writes the summary and the day results as JSON.
func WriteJSON(w io.Writer, s Summary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

type mdWriter struct {
	w   io.Writer
	err error
}

func (m *mdWriter) f(format string, a ...any) {
	if m.err == nil {
		_, m.err = fmt.Fprintf(m.w, format, a...)
	}
}

func (m *mdWriter) row(cells ...string) { m.f("| %s |\n", strings.Join(cells, " | ")) }

func (m *mdWriter) header(cells ...string) {
	m.row(cells...)
	seps := make([]string, len(cells))
	for i := range seps {
		seps[i] = "---"
	}
	m.row(seps...)
}

// money formats rupees with a sign and no paise.
func money(v float64) string { return fmt.Sprintf("%+.0f", v) }

func pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func signedPct(v float64) string { return fmt.Sprintf("%+.2f%%", v) }

// WriteMarkdown writes the study report.
func WriteMarkdown(w io.Writer, s Summary, meta Meta) error {
	m := &mdWriter{w: w}
	m.f("# Picker backtest, %s to %s\n\n", s.From, s.To)
	m.f("Generated %s by `jevtrader study`. %d trading day(s) studied, %d skipped.\n\n", meta.Generated.UTC().Format("2006-01-02 15:04 UTC"), s.OKDays, len(s.Skipped))
	if len(meta.Settings) > 0 {
		m.f("| Setting | Value |\n| --- | --- |\n")
		for _, kv := range meta.Settings {
			m.row(kv[0], kv[1])
		}
		m.f("\n")
	}
	if s.OKDays == 0 {
		m.f("No day could be studied.\n\n")
		writeSkipped(m, s)
		writeLimitations(m)
		return m.err
	}

	// 1. Totals.
	m.f("## Results\n\n")
	m.f("Net P&L after costs over the window, summed over the days. A day's buy-and-hold buys every name of the list at the window's first open and sells at its last close.\n\n")
	m.header("Strategy", "List", "Total net", "Mean per day", "Positive days", "Trades", "Round-trip hit rate", "Costs")
	m.row("buy-and-hold", "picks", money(s.Picks.Net), money(perDay(s.Picks.Net, s.Picks.Days)), fmt.Sprintf("%d/%d", s.Picks.PositiveDays, s.Picks.Days), "", "", fmt.Sprintf("%.0f", s.Picks.Costs))
	if s.Random.Days > 0 {
		m.row("buy-and-hold", "random", money(s.Random.Net), money(perDay(s.Random.Net, s.Random.Days)), fmt.Sprintf("%d/%d", s.Random.PositiveDays, s.Random.Days), "", "", fmt.Sprintf("%.0f", s.Random.Costs))
	}
	for _, d := range s.Deciders {
		m.row(d.Decider, "picks", money(d.NetPnL), money(d.MeanPerDay), fmt.Sprintf("%d/%d", d.PositiveDays, d.Days), fmt.Sprint(d.Trades), hitRate(d), fmt.Sprintf("%.0f", d.Costs))
	}
	for _, d := range s.RandomDeciders {
		m.row(d.Decider, "random", money(d.NetPnL), money(d.MeanPerDay), fmt.Sprintf("%d/%d", d.PositiveDays, d.Days), fmt.Sprint(d.Trades), hitRate(d), fmt.Sprintf("%.0f", d.Costs))
	}
	m.f("\n")

	for _, d := range append(slices.Clone(s.Deciders), s.RandomDeciders...) {
		if d.HoldSubstitutions > 0 {
			m.f("> **%s**: %d of its %d answers were HOLDs standing in for a failed call (timeout, rate limit or an invalid answer). Its results are understated by that much; rerun the affected days with `--refresh` to ask again (answers already stored are not paid for twice).\n\n", d.Decider, d.HoldSubstitutions, d.Buys+d.Sells+d.Holds)
		}
	}

	// 1b. Market-neutral.
	writeNeutral(m, s, meta)

	// 2. Is it better than chance?
	if len(s.Paired) > 0 {
		m.f("## Against buy-and-hold and against a random list\n\n")
		m.f("Day-by-day differences in net P&L. `t` is the mean difference over its standard error, treating days as independent (see the limitations).\n\n")
		m.header("Difference", "Days", "Total", "Mean per day", "Spread (sd)", "t", "Days ahead")
		for _, p := range s.Paired {
			m.row(p.Name, fmt.Sprint(p.N), money(p.Total), money(p.Mean), fmt.Sprintf("%.0f", p.SD), fmt.Sprintf("%+.2f", p.T), fmt.Sprintf("%d/%d", p.Wins, p.N))
		}
		m.f("\n")
	}

	// 3. Names.
	m.f("## Hit rates\n\n")
	m.f("**Names that rose over the window** (a stock-day with candles; before costs):\n\n")
	m.header("List", "Stock-days", "Rose", "Share", "Mean return")
	m.row("picks", fmt.Sprint(s.Picks.Names), fmt.Sprint(s.Picks.NamesUp), pct(s.Picks.UpShare), signedPct(s.Picks.MeanReturnPct))
	if s.Random.Names > 0 {
		m.row("random", fmt.Sprint(s.Random.Names), fmt.Sprint(s.Random.NamesUp), pct(s.Random.UpShare), signedPct(s.Random.MeanReturnPct))
	}
	m.f("\n")
	if len(s.Calls) > 0 {
		m.f("**BUY and SELL answers on the picks that moved the called way** (every answer, whether or not the risk gate let it trade):\n\n")
		m.header("Decider", "After", "Scored", "Right", "Hit rate", "Mean move in the called direction")
		for _, c := range s.Calls {
			m.row(c.Decider, c.Label, fmt.Sprint(c.Scored), fmt.Sprint(c.Right), pct(c.HitRate), signedPct(c.MeanMovePct))
		}
		m.f("\n")
	}

	// 4. Per day.
	writePerDay(m, s)

	// 5. Calibration.
	writeCalibration(m, s, meta)

	// 6. Signals.
	writeSignals(m, s)

	// 7. Cost.
	writeCost(m, s)
	writeNewsGaps(m, s)

	writeSkipped(m, s)
	writeLimitations(m)
	return m.err
}

func writeNeutral(m *mdWriter, s Summary, meta Meta) {
	n := s.Neutral
	m.f("## Market-neutral evidence\n\n")
	m.f("A stock's raw return is mostly the market's day. The **excess** return is the stock's return minus the mean of its day's cross-section over the same window (the same minutes): %s. Means pool the stock-days; the **95%% interval** resamples whole days (%d draws, seeded), because the stocks of one day share the market and are not independent. **Weeks positive** counts the calendar weeks whose mean excess was above zero. A mean whose interval includes zero is not evidence.\n\n", benchmarkText(n), bootstrapDraws)
	if n.ExcludedDays > 0 {
		m.f("%d day(s) had fewer than %d stocks for a cross-section and are left out of this section.\n\n", n.ExcludedDays, minBenchmark)
	}
	if len(n.BenchmarkDays) == 0 {
		m.f("No day had a cross-section.\n\n")
		return
	}
	table := func(rows []NeutralRow, day bool) {
		cols := []string{"Group", "Stock-days", "Days", "Mean return", "Mean excess", "95% interval", "Weeks positive"}
		if meta.CostPct > 0 {
			cols = append(cols, "Long, excess after costs")
		}
		m.header(cols...)
		for _, r := range rows {
			me := r.Window
			if day {
				me = r.Day
			}
			if me.N == 0 {
				m.row(r.Name, "0", "0", "n/a", "n/a", "n/a", "n/a")
				continue
			}
			row := []string{r.Name, fmt.Sprint(me.N), fmt.Sprint(me.Days), signedPct(me.MeanRaw), signedPct(me.Mean), interval(me.CI), fmt.Sprintf("%d/%d", me.PositiveWeeks, me.Weeks)}
			if meta.CostPct > 0 {
				row = append(row, signedPct(me.Mean-meta.CostPct))
			}
			m.row(row...)
		}
		m.f("\n")
	}
	m.f("**Picks and the random list, over the window** (open of its first bar to close of its last):\n\n")
	table(n.Lists, false)
	m.f("**Picks and the random list, to the day's close:**\n\n")
	table(n.Lists, true)

	m.f("### The news signal\n\n")
	if n.Universe {
		m.f("Every screened stock-day, not only the picks, grouped by the sign of its net news signal that morning (Jev's score of the items about it, aged and summed; negative news included). \"Scored, net zero\" had items that cancelled; \"no scored news item\" is most of the pool. The last row is the day-by-day difference of the two groups' mean excess, over the days that had both; its `Stock-days` are days.\n\n")
	} else {
		m.f("Only the stocks the study measured (the picks and the random list), grouped by the sign of their net news signal that morning. Run with `--universe` to measure every screened stock and read the signal over the whole pool, negative news included. The last row is the day-by-day difference of the two groups' mean excess, over the days that had both; its `Stock-days` are days.\n\n")
	}
	m.f("*Over the window:*\n\n")
	table(append(slices.Clone(n.News), n.Spread), false)
	m.f("*To the day's close:*\n\n")
	table(append(slices.Clone(n.News), n.Spread), true)

	if len(n.Weeks) > 0 {
		m.f("### Week by week\n\n")
		m.f("Mean excess over the window by calendar week (the Monday), with the stock-days behind it: is the effect in most weeks, or in a few?\n\n")
		m.header("Week of", "Days", "Picks", "News above 0", "News below 0")
		for _, w := range n.Weeks {
			m.row(w.Week, fmt.Sprint(w.Days), weekCell(w.Picks), weekCell(w.Positive), weekCell(w.Negative))
		}
		m.f("\n")
	}
}

func benchmarkText(n Neutral) string {
	var parts []string
	for _, k := range []string{BenchmarkPool, BenchmarkRandom, BenchmarkStudied} {
		if c := n.BenchmarkDays[k]; c > 0 {
			parts = append(parts, fmt.Sprintf("%s on %d day(s)", k, c))
		}
	}
	return strings.Join(parts, ", ")
}

func interval(i Interval) string {
	if !i.OK {
		return fmt.Sprintf("n/a (under %d days)", minCIDays)
	}
	return fmt.Sprintf("%s to %s", signedPct(i.Lo), signedPct(i.Hi))
}

func weekCell(c WeekCell) string {
	if c.N == 0 {
		return "–"
	}
	return fmt.Sprintf("%s (%d)", signedPct(c.Mean), c.N)
}

func perDay(total float64, days int) float64 {
	if days == 0 {
		return 0
	}
	return total / float64(days)
}

func hitRate(d DeciderSummary) string {
	if d.Trades == 0 {
		return "n/a"
	}
	return pct(d.HitRate)
}

func writePerDay(m *mdWriter, s Summary) {
	m.f("## Per day\n\n")
	cols := []string{"Day", "Picks buy-and-hold", "Random buy-and-hold"}
	for _, d := range s.Deciders {
		cols = append(cols, d.Decider+" (picks)")
	}
	for _, d := range s.RandomDeciders {
		cols = append(cols, d.Decider+" (random)")
	}
	cols = append(cols, "Picks up", "Random up")
	m.header(cols...)
	for _, d := range s.Days {
		if d.Status != StatusOK {
			continue
		}
		row := []string{d.Date, money(d.Picks.BuyHold.Net), "n/a"}
		if d.Random != nil {
			row[2] = money(d.Random.BuyHold.Net)
		}
		for _, dec := range s.Deciders {
			row = append(row, runNet(&d.Picks, dec.Decider))
		}
		for _, dec := range s.RandomDeciders {
			row = append(row, runNet(d.Random, dec.Decider))
		}
		row = append(row, upShare(&d.Picks), upShare(d.Random))
		m.row(row...)
	}
	m.f("\n")
}

func runNet(l *ListResult, decider string) string {
	if l == nil {
		return "n/a"
	}
	if r, ok := l.Run(decider); ok {
		return money(r.NetPnL)
	}
	return "n/a"
}

func upShare(l *ListResult) string {
	if l == nil || l.BuyHold.NamesWithData == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d", l.BuyHold.NamesUp, l.BuyHold.NamesWithData)
}

func writeCalibration(m *mdWriter, s Summary, meta Meta) {
	m.f("## Calibration\n\n")
	if meta.Horizon > 0 {
		m.f("The probability a decider gave each BUY or SELL against how often the price then moved that way %d bar(s) later, over all days of the picks. Brier is the mean squared gap (0.25 is what always saying 50%% scores).\n\n", meta.Horizon)
	}
	any := false
	for _, c := range s.Calibration {
		if c.Samples == 0 {
			continue
		}
		any = true
		m.f("**%s**: %d calls, Brier %.3f\n\n", c.Decider, c.Samples, c.Brier)
		m.header("Probability", "Calls", "Mean predicted", "Moved that way")
		for _, b := range c.Bins {
			m.row(fmt.Sprintf("%.1f–%.1f", b.Lo, b.Hi), fmt.Sprint(b.N), pct(b.Predicted), pct(b.Realized))
		}
		m.f("\n")
	}
	if !any {
		m.f("No BUY or SELL call was made.\n\n")
	}
}

func writeSignals(m *mdWriter, s Summary) {
	m.f("## Which signals went with the window return\n\n")
	m.f("Each signal of the morning's screen (in [-1, 1]) against the stock's return from the window's open to its close. `r` is Pearson and `ρ` Spearman over all stock-days; `daily IC` is the mean over days of that day's rank correlation (days with at least %d stocks), with its t and its 95%% interval resampling days. The last columns are the mean return and count of the stocks whose signal was above zero, below zero, and zero or not read. The gap signal is absent on past days.\n\n", minDailyNames)
	for _, population := range []string{"all studied stocks", "picks"} {
		if population == "picks" {
			m.f("Picks only (the population the live bot trades):\n\n")
		} else {
			if s.Neutral.Universe {
				m.f("Every stock measured (the whole screened pool, the picks and the random list included):\n\n")
			} else {
				m.f("Picks and the random list together (the picks are a selection from the pool, so this population has both ends of the score):\n\n")
			}
		}
		m.header("Signal", "Stock-days", "r", "ρ", "Daily IC", "IC 95% interval", "IC days", "IC t", "Above 0", "Below 0", "Zero / not read")
		for _, st := range s.Signals {
			if st.Population != population {
				continue
			}
			if st.N == 0 {
				m.row(st.Signal, "0", "n/a", "n/a", "n/a", "n/a", "0", "n/a", bucket(st.Above), bucket(st.Below), bucket(st.None))
				continue
			}
			r, rho := "n/a", "n/a"
			if st.HasCorr {
				r, rho = fmt.Sprintf("%+.3f", st.Pearson), fmt.Sprintf("%+.3f", st.Spearman)
			}
			ic, ict := "n/a", "n/a"
			if st.ICDays > 0 {
				ic, ict = fmt.Sprintf("%+.3f", st.MeanDailyIC), fmt.Sprintf("%+.2f", st.ICT)
			}
			ici := "n/a"
			if st.ICCI.OK {
				ici = fmt.Sprintf("%+.3f to %+.3f", st.ICCI.Lo, st.ICCI.Hi)
			}
			m.row(st.Signal, fmt.Sprint(st.N), r, rho, ic, ici, fmt.Sprint(st.ICDays), ict, bucket(st.Above), bucket(st.Below), bucket(st.None))
		}
		m.f("\n")
	}
}

func bucket(b Bucket) string {
	if b.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%s (%d)", signedPct(b.MeanReturnPct), b.N)
}

func writeCost(m *mdWriter, s Summary) {
	u := s.Usage
	m.f("## Jev token cost\n\n")
	m.header("", "Calls", "Input tokens", "Output tokens", "At list price")
	m.row("Decisions, paid", fmt.Sprint(u.Calls), fmt.Sprint(u.InputTokens), fmt.Sprint(u.OutputTokens), usd(u.InputTokens))
	m.row("Decisions, answered from the store", fmt.Sprint(u.CacheHits), fmt.Sprint(u.SavedInputTokens), fmt.Sprint(u.SavedOutputTokens), usd(u.SavedInputTokens)+" saved")
	m.row("News scoring in the screens", "", fmt.Sprint(s.ScreenInputTokens), fmt.Sprint(s.ScreenOutputTokens), usd(s.ScreenInputTokens))
	m.f("\nList price is $42 per billion input tokens, output free (jev-1.13.0). Paid counts what the run that computed each day was billed; a rerun answers from the store and costs nothing, and a day that was already stored is not asked again.\n\n")
}

func writeNewsGaps(m *mdWriter, s Summary) {
	if len(s.NewsGaps) == 0 {
		return
	}
	m.f("## News scoring gaps\n\n")
	m.f("Jev failed on some news items on these days (a rate limit or an outage that outlasted the retries), so their news signal is incomplete. The items it did score are stored: run again with `--refresh` and only the ones that failed are asked.\n\n")
	for _, g := range s.NewsGaps {
		m.f("- %s\n", g)
	}
	m.f("\n")
}

func usd(inputTokens int) string {
	return fmt.Sprintf("$%.4f", float64(inputTokens)*screen.JevPricePerToken)
}

func writeSkipped(m *mdWriter, s Summary) {
	if len(s.Skipped) == 0 {
		return
	}
	m.f("## Skipped days\n\n")
	for _, d := range s.Skipped {
		m.f("- %s: %s\n", d.Date, d.Reason)
	}
	m.f("\n")
}

func writeLimitations(m *mdWriter) {
	m.f("## Limitations\n\n")
	for _, l := range Limitations {
		m.f("- %s\n", l)
	}
	m.f("\n")
}
