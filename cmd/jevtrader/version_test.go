package main

import (
	"context"
	"testing"
)

func TestVersionPrintsStampedValue(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "0123456789ab"
	e, stdout, _ := testEnv(nil)
	if code := run(context.Background(), []string{"version"}, e); code != 0 || stdout.String() != "0123456789ab\n" {
		t.Errorf("code %d, output %q", code, stdout.String())
	}
}
