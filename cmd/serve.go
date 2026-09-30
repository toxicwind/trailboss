package main

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"
	"github.com/toxicwind/trailboss/serve"
)

// webUI is the embedded web UI: vanilla HTML/JS/CSS, no build step, served by
// `trailboss serve`. The fs.Sub in newServeCmd strips the webui/ prefix so the
// server sees index.html at its root.
//
//go:embed webui/index.html webui/app.js webui/style.css
var webUI embed.FS

func newServeCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the trailboss web UI and JSON API",
		Long: "serve starts a local web UI for the whole trailboss workflow: browse\n" +
			"selection lockfiles, run new roundups, mirror/scan/apply as background\n" +
			"jobs, and watch their logs stream live. It serves on --addr (default\n" +
			"127.0.0.1:25250) and reads its GitHub token from TRAILBOSS_PAT in the\n" +
			"server environment — the token is never exposed through the API.\n\n" +
			"Jobs call trailboss's Go packages directly (select/mirror/scan/apply)\n" +
			"with a log-capturing runner; the server never shells out to the\n" +
			"trailboss binary. Jobs are in-memory: a restart loses them.",
		RunE: func(cmd *cobra.Command, args []string) error {
			sub, err := fs.Sub(webUI, "webui")
			if err != nil {
				return fmt.Errorf("embedded web UI is incomplete: %w", err)
			}
			return serve.Run(cmd.Context(), serve.Options{Addr: addr, WebFS: sub})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:25250", "address to listen on, e.g. 127.0.0.1:25250")
	return cmd
}
