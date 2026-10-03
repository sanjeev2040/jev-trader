package main

import (
	"context"
	"fmt"
)

// version is the git SHA the image build stamps in with
// -ldflags "-X main.version=<sha>" (see Dockerfile). A plain `go build` leaves
// it "dev".
var version = "dev"

func runVersion(_ context.Context, _ []string, e env) int {
	fmt.Fprintln(e.stdout, version)
	return 0
}
