package cli

import (
	"github.com/spf13/cobra"
)

// newCompletionCmd defines `apipod completion <shell>`. It replaces cobra's
// generated command, which exits 0 without output for an unknown shell.
func (a *App) newCompletionCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion",
		Short: "Generate a shell completion script",
		Long: `Generate a completion script for bash, zsh, fish or PowerShell.

  source <(apipod completion bash)                              # bash, current shell
  apipod completion zsh > "${fpath[1]}/_apipod"                 # zsh
  apipod completion fish > ~/.config/fish/completions/apipod.fish
  apipod completion powershell | Out-String | Invoke-Expression`,
		Args: positional(noArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	shells := []struct {
		name string
		gen  func(cmd *cobra.Command) error
	}{
		{"bash", func(cmd *cobra.Command) error { return root.GenBashCompletionV2(cmd.OutOrStdout(), true) }},
		{"zsh", func(cmd *cobra.Command) error { return root.GenZshCompletion(cmd.OutOrStdout()) }},
		{"fish", func(cmd *cobra.Command) error { return root.GenFishCompletion(cmd.OutOrStdout(), true) }},
		{"powershell", func(cmd *cobra.Command) error { return root.GenPowerShellCompletionWithDesc(cmd.OutOrStdout()) }},
	}
	for _, shell := range shells {
		cmd.AddCommand(&cobra.Command{
			Use:   shell.name,
			Short: "Generate the completion script for " + shell.name,
			Args:  positional(noArgs),
			RunE:  func(cmd *cobra.Command, _ []string) error { return shell.gen(cmd) },
		})
	}
	return cmd
}
