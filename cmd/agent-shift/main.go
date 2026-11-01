// Command agent-shift relays Slack threads to Claude Code / Codex sessions running locally.
package main

import (
	"os"

	"github.com/daffirds/agent-shift/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
