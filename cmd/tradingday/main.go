// Command tradingday tells the market-hours workflow whether the NSE trades on
// a date, using deploy/nse-holidays.yaml.
//
//	tradingday [-file deploy/nse-holidays.yaml] [-date YYYY-MM-DD]
//
// With no -date it uses today's date in IST. It prints a one-line reason and
// exits 0 when the market is open, 1 when it is closed and 2 on any error.
// With -github-output it also appends open=true|false to that file, the way
// $GITHUB_OUTPUT expects.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sanjeev2040/jev-trader/internal/nsecal"
)

func main() {
	os.Exit(run(os.Args[1:], time.Now, os.Stdout, os.Stderr))
}

func run(args []string, now func() time.Time, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tradingday", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "deploy/nse-holidays.yaml", "holiday calendar")
	date := fs.String("date", "", "date to check, YYYY-MM-DD (default: today in IST)")
	ghOutput := fs.String("github-output", "", "append open=true|false to this file ($GITHUB_OUTPUT)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *date == "" {
		ist := time.FixedZone("IST", 5*3600+30*60)
		*date = now().In(ist).Format("2006-01-02")
	}

	f, err := os.Open(*file)
	if err != nil {
		fmt.Fprintf(stderr, "tradingday: %v\n", err)
		return 2
	}
	defer f.Close()
	cal, err := nsecal.Parse(f)
	if err != nil {
		fmt.Fprintf(stderr, "tradingday: %v\n", err)
		return 2
	}
	res, err := cal.Check(*date)
	if err != nil {
		fmt.Fprintf(stderr, "tradingday: %v\n", err)
		return 2
	}
	if res.Warning != "" {
		fmt.Fprintf(stderr, "tradingday: warning: %s\n", res.Warning)
	}
	state := "closed"
	if res.Open {
		state = "open"
	}
	fmt.Fprintf(stdout, "%s: NSE %s (%s)\n", *date, state, res.Reason)
	if *ghOutput != "" {
		out, err := os.OpenFile(*ghOutput, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			fmt.Fprintf(stderr, "tradingday: %v\n", err)
			return 2
		}
		_, werr := fmt.Fprintf(out, "open=%t\n", res.Open)
		if cerr := out.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			fmt.Fprintf(stderr, "tradingday: %v\n", werr)
			return 2
		}
	}
	if !res.Open {
		return 1
	}
	return 0
}
