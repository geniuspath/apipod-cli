package cli

import (
	"fmt"
	"io"
	goruntime "runtime"

	"github.com/spf13/cobra"
)

func (a *App) newVersionCmd(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  positional(noArgs),
		RunE: a.run(flags, func(rt *runtime, _ []string) error {
			info := map[string]string{
				"version": a.Build.Version,
				"commit":  a.Build.Commit,
				"date":    a.Build.Date,
				"go":      goruntime.Version(),
				"os":      goruntime.GOOS,
				"arch":    goruntime.GOARCH,
			}
			return rt.p.Result(info, func(w io.Writer) {
				fmt.Fprintf(w, "apipod %s (%s, %s, %s/%s)\n", a.Build.Version, a.Build.Commit, a.Build.Date, goruntime.GOOS, goruntime.GOARCH)
			})
		}),
	}
}
