package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/config"
)

func (a *App) newConfigCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and change CLI settings",
		Long: `Read and change CLI settings stored in config.toml.

Keys:
  defaults.image_model   model used by "apipod image" when -m is omitted
  defaults.video_model   model used by "apipod video" when -m is omitted
  defaults.output        "human" (default) or "json"
  spend.confirm_above    estimated cost in USD above which a generation needs
                         confirmation, --yes or --max-cost (default 1)

Credentials are never stored in this file.`,
		Args: positional(noArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	get := &cobra.Command{
		Use:   "get <key>",
		Short: "Print one setting",
		Args:  positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error {
			value, err := rt.cfg.Get(args[0])
			if err != nil {
				return usagef("%v", err)
			}
			return rt.p.Result(map[string]string{"key": args[0], "value": value}, func(w io.Writer) {
				fmt.Fprintln(w, value)
			})
		}),
	}

	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change one setting",
		Args:  positional(cobra.ExactArgs(2)),
		RunE: a.run(flags, func(rt *runtime, args []string) error {
			next, err := rt.cfg.Set(args[0], args[1])
			if err != nil {
				return usagef("%v", err)
			}
			if err := config.Save(rt.cfgDir, next); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			value, _ := next.Get(args[0])
			return rt.p.Result(map[string]string{"key": args[0], "value": value}, func(w io.Writer) {
				fmt.Fprintf(w, "%s = %s\n", args[0], value)
			})
		}),
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "Print every setting",
		Args:  positional(noArgs),
		RunE: a.run(flags, func(rt *runtime, _ []string) error {
			values := map[string]string{}
			pairs := []string{}
			for _, key := range config.Keys() {
				value, _ := rt.cfg.Get(key)
				values[key] = value
				pairs = append(pairs, key, value)
			}
			path := filepath.Join(rt.cfgDir, "config.toml")
			return rt.p.Result(map[string]any{"path": path, "values": values}, func(w io.Writer) {
				rt.p.KV(w, pairs...)
				fmt.Fprintf(w, "\n%s\n", rt.p.Dim(path))
			})
		}),
	}

	cmd.AddCommand(get, set, list)
	return cmd
}
