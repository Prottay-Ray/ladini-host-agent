package image

import _ "embed"

// IsolationCheck is the in-guest half of the isolation test matrix.
//
// It is embedded rather than read from disk so that it ships inside the agent
// binary and is baked into the golden image at build time: the script that
// verifies a node is the one that was compiled into the agent that built it,
// not whatever happens to be in the working directory.
//
//go:embed assets/isolation-check.sh
var IsolationCheck string

// GuestScriptPath is where the golden image installs it.
const GuestScriptPath = "/usr/local/lib/ladini/isolation-check.sh"
