package snapshot

import (
	"fmt"

	"github.com/sanjeev2040/jev-trader/internal/domain"
)

// Aliases assigns each instrument an opaque alias ("I01", "I02", ...) in the
// order first seen. Create one per run so aliases carry no meaning across
// runs. It is not safe for concurrent use; the engine loop owns it.
type Aliases struct {
	byInstrument map[domain.Instrument]string
}

// NewAliases returns an empty alias table.
func NewAliases() *Aliases {
	return &Aliases{byInstrument: make(map[domain.Instrument]string)}
}

// Alias returns inst's alias, assigning the next one on first use.
func (a *Aliases) Alias(inst domain.Instrument) string {
	if alias, ok := a.byInstrument[inst]; ok {
		return alias
	}
	alias := fmt.Sprintf("I%02d", len(a.byInstrument)+1)
	a.byInstrument[inst] = alias
	return alias
}
