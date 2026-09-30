package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/toxicwind/trailboss/upstream"
)

// resolveForks returns the fork registry. WorkDir defaults to
// $TRAILBOSS_FORKS_DIR/<name> if not set per-fork.
func resolveForks() []upstream.Fork {
	forks := upstream.DefaultForks
	base := os.Getenv("TRAILBOSS_FORKS_DIR")
	for i := range forks {
		if forks[i].WorkDir == "" && base != "" {
			forks[i].WorkDir = base + "/" + forks[i].Name
		}
	}
	return forks
}

func findFork(forks []upstream.Fork, name string) (*upstream.Fork, error) {
	for i := range forks {
		if forks[i].Name == name {
			return &forks[i], nil
		}
	}
	return nil, fmt.Errorf("unknown fork %q (known: herd, tau, roundup, trailboss)", name)
}

func newUpstreamCmd() *cobra.Command {
	var dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "upstream (status | merge <fork> | watch | init)",
		Short: "Own our forks' upstream relationships — merge, test, report (never push to main)",
		Long: "upstream is trailboss's primary feature: agentic ownership of upstream patch\n" +
			"relationships. It watches our renamed forks' upstreams, merges on fresh\n" +
			"branches, runs test suites, and reports. It NEVER pushes to main — it\n" +
			"prepares, verifies, and reports. The human gives the final go.\n\n" +
			"  trailboss upstream status        — show divergence of each fork vs its upstream\n" +
			"  trailboss upstream merge <fork>  — merge upstream onto a fresh branch (never main)\n" +
			"  trailboss upstream watch         — check all forks, merge, test, report (cron-friendly)\n" +
			"  trailboss upstream init          — add upstream remotes to local checkouts\n\n" +
			"Set TRAILBOSS_FORKS_DIR to the parent dir of local checkouts\n" +
			"(e.g. /home/toxic/forks, containing herd/, tau/, ...).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "status":
				return runUpstreamStatus(cmd, jsonOut)
			case "merge":
				if len(args) < 2 {
					return fmt.Errorf("upstream merge needs a fork name: herd, tau, roundup, trailboss")
				}
				return runUpstreamMerge(cmd, args[1], dryRun, jsonOut)
			case "watch":
				return runUpstreamWatch(cmd, dryRun, jsonOut)
			case "init":
				return runUpstreamInit(cmd)
			default:
				return fmt.Errorf("unknown upstream action %q (want: status, merge, watch, init)", args[0])
			}
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would happen without creating branches")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return cmd
}

func runUpstreamStatus(cmd *cobra.Command, jsonOut bool) error {
	var statuses []*upstream.Status
	for _, f := range resolveForks() {
		if f.WorkDir == "" {
			continue
		}
		s, err := upstream.GetStatus(f)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		statuses = append(statuses, s)
	}
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(statuses)
	}
	for _, s := range statuses {
		flag := "✅"
		if s.NeedsMerge {
			flag = "🔀"
		}
		sha := s.UpstreamSHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %-10s behind=%d ahead=%d upstream=%s\n",
			flag, s.Fork, s.Behind, s.Ahead, sha)
	}
	return nil
}

func runUpstreamMerge(cmd *cobra.Command, forkName string, dryRun, jsonOut bool) error {
	f, err := findFork(resolveForks(), forkName)
	if err != nil {
		return err
	}
	if f.WorkDir == "" {
		return fmt.Errorf("no local checkout for %s (set TRAILBOSS_FORKS_DIR)", f.Name)
	}
	result, err := upstream.Merge(*f, dryRun)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	fmt.Fprintln(cmd.OutOrStdout(), result.Report)
	return nil
}

func runUpstreamWatch(cmd *cobra.Command, dryRun, jsonOut bool) error {
	wr, err := upstream.Watch(resolveForks(), dryRun)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(wr)
	}
	fmt.Fprintln(cmd.OutOrStdout(), upstream.FormatFleetReport(wr))
	return nil
}

func runUpstreamInit(cmd *cobra.Command) error {
	for _, f := range resolveForks() {
		if f.WorkDir == "" {
			fmt.Fprintf(cmd.OutOrStdout(), "⏭️  %s: no local checkout — skipping\n", f.Name)
			continue
		}
		if err := upstream.EnsureUpstreamRemote(f); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✅ %s: upstream -> %s\n", f.Name, f.UpstreamURL)
	}
	return nil
}
