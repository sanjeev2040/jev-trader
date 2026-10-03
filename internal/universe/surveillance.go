package universe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Surveillance is the set of stocks NSE currently lists under its Additional
// Surveillance Measure (long and short term) or Graded Surveillance Measure
// frameworks, with the stage NSE gives. Such stocks trade under price bands,
// higher margins or trade-for-trade, which makes them unsuitable for intraday.
type Surveillance struct {
	// Reasons maps a symbol to a short text such as "ASM LTASM - I (13)".
	Reasons map[string]string
}

// Size is the number of stocks listed.
func (s Surveillance) Size() int { return len(s.Reasons) }

// FetchSurveillance loads NSE's ASM and GSM reports (cached for the IST day).
// Both are JSON: reportASM is {"longterm":{"data":[...]},"shortterm":{"data":[...]}}
// and reportGSM is a bare array; each row has symbol and survCode. If either
// report cannot be read it returns the other's stocks together with an error
// naming what is missing, so the caller can carry on and say so.
func FetchSurveillance(ctx context.Context, cacheDir string, now time.Time, fetch Fetcher) (Surveillance, error) {
	out := Surveillance{Reasons: map[string]string{}}
	var errs []error
	if err := loadASM(ctx, cacheDir, now, fetch, out.Reasons); err != nil {
		errs = append(errs, fmt.Errorf("asm: %w", err))
	}
	if err := loadGSM(ctx, cacheDir, now, fetch, out.Reasons); err != nil {
		errs = append(errs, fmt.Errorf("gsm: %w", err))
	}
	switch len(errs) {
	case 0:
		return out, nil
	case 1:
		return out, errs[0]
	}
	return out, fmt.Errorf("%v; %v", errs[0], errs[1])
}

type surveillanceRow struct {
	Symbol   string `json:"symbol"`
	SurvCode string `json:"survCode"`
}

func loadASM(ctx context.Context, dir string, now time.Time, fetch Fetcher, into map[string]string) error {
	r, err := cached(ctx, dir, now, "asm", ".json", fetch)
	if err != nil {
		return err
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	var rep struct {
		Longterm  struct{ Data []surveillanceRow } `json:"longterm"`
		Shortterm struct{ Data []surveillanceRow } `json:"shortterm"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	rows := append(rep.Longterm.Data, rep.Shortterm.Data...)
	if len(rows) == 0 {
		return fmt.Errorf("report lists no stocks (format changed?)")
	}
	for _, row := range rows {
		if row.Symbol != "" {
			into[row.Symbol] = "ASM " + row.SurvCode
		}
	}
	return nil
}

func loadGSM(ctx context.Context, dir string, now time.Time, fetch Fetcher, into map[string]string) error {
	r, err := cached(ctx, dir, now, "gsm", ".json", fetch)
	if err != nil {
		return err
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	var rows []surveillanceRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("report lists no stocks (format changed?)")
	}
	for _, row := range rows {
		if row.Symbol != "" {
			into[row.Symbol] = "GSM " + row.SurvCode
		}
	}
	return nil
}
