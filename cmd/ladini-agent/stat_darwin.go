package main

import "syscall"

// syscallStat exposes the allocated block count, so the CLI can show how much
// space a sparse image or an APFS clone actually occupies -- the difference
// between apparent and real size is the whole point of cloning.
type syscallStat = syscall.Stat_t
