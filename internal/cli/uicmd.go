package cli

import (
	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/webui"
)

func uiCmd() *cobra.Command {
	var (
		addr      string
		noBrowser bool
	)
	cmd := &cobra.Command{
		Use:     "ui",
		Aliases: []string{"setup"},
		Short:   "Open the configuration UI: Slack workspaces, agents, daemon",
		Long: `Opens a small web app on 127.0.0.1 to configure agent-shift: your Slack app and token, your
agents and the daemon. It stops when you
press Ctrl+C. Saved changes are applied to a running daemon right away.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return webui.Serve(cmd.Context(), webui.Options{Addr: addr, OpenBrowser: !noBrowser})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7457", "address to listen on (loopback only)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening it")
	return cmd
}
