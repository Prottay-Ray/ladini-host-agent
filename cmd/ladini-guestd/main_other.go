//go:build !linux

// ladini-guestd runs only inside the Linux guest. This stub exists so that
// `go build ./...` and `go vet ./...` work on the developer's Mac; build the
// real binary with `make guestd`.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ladini-guestd runs inside the Linux guest only; build it with `make guestd`")
	os.Exit(1)
}
