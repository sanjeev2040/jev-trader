package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// maxSeq bounds the files one window can hold, one per writer run.
const maxSeq = 999

// BarWriter writes bars of one kind to Parquet files that rotate every
// RotateEvery of bar time. It is not safe for concurrent use.
type BarWriter struct {
	w *writer[barRow]
}

// NewBarWriter writes bars of kind (KindBars or KindBackfill) under root.
func NewBarWriter(root, kind string) (*BarWriter, error) {
	if kind != KindBars && kind != KindBackfill {
		return nil, fmt.Errorf("store: %q is not a bar kind", kind)
	}
	return &BarWriter{w: &writer[barRow]{root: root, kind: kind, schema: BarSchemaVersion}}, nil
}

// Write appends bars. A bar whose closing window differs from the open file's
// closes that file and opens another.
func (w *BarWriter) Write(bars ...domain.Bar) error {
	for _, b := range bars {
		if b.Interval <= 0 || b.Interval%time.Second != 0 {
			return fmt.Errorf("store: bar interval %v is not a whole number of seconds", b.Interval)
		}
		if err := w.w.write(barTime(b), barToRow(b)); err != nil {
			return err
		}
	}
	return nil
}

// Rotate closes the open file if now is past the end of its window. Call it
// from the clock so a quiet stream still publishes its last file.
func (w *BarWriter) Rotate(now time.Time) error { return w.w.rotate(now) }

// Close closes the open file, if any.
func (w *BarWriter) Close() error { return w.w.close() }

// writer is the shared rotating Parquet writer for one kind of row.
type writer[R any] struct {
	root, kind, schema string
	cur                *openFile[R]
}

type openFile[R any] struct {
	window     time.Time // window start, IST
	tmp, final string
	f          *os.File
	pw         *parquet.GenericWriter[R]
}

func (w *writer[R]) write(ts time.Time, row R) error {
	window := ts.In(ist).Truncate(RotateEvery)
	if w.cur != nil && !w.cur.window.Equal(window) {
		if err := w.close(); err != nil {
			return err
		}
	}
	if w.cur == nil {
		if err := w.open(window); err != nil {
			return err
		}
	}
	if _, err := w.cur.pw.Write([]R{row}); err != nil {
		return fmt.Errorf("store: write %s: %w", w.cur.tmp, err)
	}
	return nil
}

func (w *writer[R]) rotate(now time.Time) error {
	if w.cur != nil && !now.Before(w.cur.window.Add(RotateEvery)) {
		return w.close()
	}
	return nil
}

func (w *writer[R]) open(window time.Time) error {
	dir := filepath.Join(w.root, w.kind, "date="+window.Format(time.DateOnly))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	for seq := 0; seq <= maxSeq; seq++ {
		name := fmt.Sprintf("%s-%s-%03d.parquet", w.kind, window.Format("1504"), seq)
		final := filepath.Join(dir, name)
		tmp := filepath.Join(dir, "."+name+".tmp")
		if _, err := os.Lstat(final); !errors.Is(err, fs.ErrNotExist) {
			continue // taken, or unreadable: try the next name
		}
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue // left by a crashed run; never reuse it
		}
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		pw := parquet.NewGenericWriter[R](f,
			parquet.Compression(&parquet.Zstd),
			parquet.KeyValueMetadata(SchemaKey, w.schema),
			parquet.KeyValueMetadata(KindKey, w.kind),
		)
		w.cur = &openFile[R]{window: window, tmp: tmp, final: final, f: f, pw: pw}
		return nil
	}
	return fmt.Errorf("store: no free file name in %s for window %s", dir, window.Format("1504"))
}

// close finishes the open file and publishes it under its final name.
func (w *writer[R]) close() error {
	cur := w.cur
	if cur == nil {
		return nil
	}
	w.cur = nil
	err := cur.pw.Close()
	if err == nil {
		err = cur.f.Sync()
	}
	if cerr := cur.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(cur.tmp, cur.final)
	}
	if err != nil {
		return fmt.Errorf("store: close %s: %w", cur.final, err)
	}
	return nil
}
