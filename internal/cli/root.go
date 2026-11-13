// Package cli wires the apipod commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/api"
	"github.com/geniuspath/apipod-cli/internal/auth"
	"github.com/geniuspath/apipod-cli/internal/config"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/output"
)

// Environment variables read by the CLI.
const (
	EnvBaseURL    = "APIPOD_BASE_URL"
	EnvConsoleURL = "APIPOD_CONSOLE_URL"
)

// BuildInfo is stamped into the binary by the release build.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// App is one CLI invocation. The fields are the process boundary — streams,
// build metadata and the few side effects tests need to replace.
type App struct {
	In    io.Reader
	Out   io.Writer
	Err   io.Writer
	Build BuildInfo

	// OpenBrowser opens a URL in the user's browser.
	OpenBrowser func(url string) error
	// NewStore builds the credential store for a config directory.
	NewStore func(dir string) *auth.Store
	// DownloadClient is the base HTTP client for result downloads (nil for the default).
	DownloadClient *http.Client
	// PollInterval overrides the job polling interval (0 for the default).
	PollInterval time.Duration
	// RetryWait overrides the API retry backoff (0 for the default).
	RetryWait time.Duration
	// Interactive overrides terminal detection (nil to detect).
	Interactive *bool
}

// NewApp returns an App bound to the process's standard streams.
func NewApp(build BuildInfo) *App {
	return &App{
		In:          os.Stdin,
		Out:         os.Stdout,
		Err:         os.Stderr,
		Build:       build,
		OpenBrowser: openBrowser,
		NewStore:    auth.NewStore,
	}
}

// globalFlags are accepted by every command.
type globalFlags struct {
	json    bool
	quiet   bool
	noColor bool
	debug   bool
	noWait  bool
	yes     bool
	output  string
	maxCost float64
}

// runtime is the per-invocation state shared by commands.
type runtime struct {
	app    *App
	flags  *globalFlags
	cmd    *cobra.Command
	p      *output.Printer
	cfgDir string
	cfg    config.Config
	debug  *api.DebugLogger
	store  *auth.Store
}

// Run executes the CLI with args and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	api.Version = a.Build.Version
	flags := &globalFlags{}
	root := a.newRoot(flags)
	root.SetArgs(args)
	root.SetIn(a.In)
	root.SetOut(a.Out)
	root.SetErr(a.Err)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitcode.OK
	}
	if isUnknownCommand(err) {
		// cobra's message repeats the argument verbatim; ours does not.
		err = usagef("unknown command. Run `apipod --help` for the list of commands")
	}
	code := exitCodeFor(err)
	a.printError(flags, args, err, code)
	return code
}

// exitCodeFor maps an error to the documented exit code table.
func exitCodeFor(err error) int {
	if errors.Is(err, context.Canceled) {
		return exitcode.General
	}
	return exitcode.From(err)
}

// isUnknownCommand recognises cobra's "unknown command" error, which arrives
// unwrapped and quotes the argument.
func isUnknownCommand(err error) bool {
	return strings.HasPrefix(err.Error(), "unknown command")
}

// printError writes the failure to stderr: a JSON object in --json mode, a
// plain line otherwise. stdout is left untouched so piped data stays clean.
func (a *App) printError(flags *globalFlags, args []string, err error, code int) {
	msg := output.Sanitize(err.Error())
	if errors.Is(err, context.Canceled) {
		msg = "interrupted"
	}
	if flags.json || wantsJSON(args) {
		body := map[string]any{"message": msg, "exit_code": code}
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			if apiErr.Code != "" {
				body["code"] = apiErr.Code
			}
			body["http_status"] = apiErr.HTTPStatus
		}
		var jobErr *jobError
		if errors.As(err, &jobErr) {
			body["job_id"] = jobErr.JobID
		}
		enc := json.NewEncoder(a.Err)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{"error": body})
		return
	}
	fmt.Fprintln(a.Err, "error: "+msg)
}

// wantsJSON covers failures that happen before flags are parsed.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "--json=true" {
			return true
		}
	}
	return false
}

func (a *App) newRoot(flags *globalFlags) *cobra.Command {
	root := &cobra.Command{
		Use:   "apipod",
		Short: "Generate images and video with APIPod from the terminal",
		Long: `apipod is the command-line client for APIPod (https://www.apipod.ai).

It is built for people and for agents alike: add --json to any command for
machine-readable output on stdout, and branch on the exit code.

Exit codes:
  0 success        3 authentication   6 billing (balance, --max-cost, spend guard)
  1 general error  4 rate limited     7 timeout
  2 usage error    5 not found        8 validation failed`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return flagError(cmd, err)
	})

	root.SetHelpCommand(a.newHelpCmd(root))

	pf := root.PersistentFlags()
	pf.BoolVar(&flags.json, "json", false, "print machine-readable JSON on stdout")
	pf.BoolVarP(&flags.quiet, "quiet", "q", false, "suppress progress output")
	pf.BoolVar(&flags.noColor, "no-color", false, "disable colors")
	pf.BoolVar(&flags.debug, "debug", false, "log HTTP requests to stderr (credentials are redacted)")
	pf.BoolVar(&flags.noWait, "no-wait", false, "return the job id without waiting for the result")
	pf.StringVarP(&flags.output, "output", "o", "", "directory for downloaded results")
	pf.Float64Var(&flags.maxCost, "max-cost", 0, "refuse to start a generation estimated above this many USD")
	pf.BoolVarP(&flags.yes, "yes", "y", false, "skip confirmation prompts")

	root.AddCommand(
		a.newAuthCmd(flags),
		a.newModelsCmd(flags),
		a.newCostCmd(flags),
		a.newGenerateCmd(flags, api.KindImage),
		a.newGenerateCmd(flags, api.KindVideo),
		a.newJobCmd(flags),
		a.newUploadCmd(flags),
		a.newAssetsCmd(flags),
		a.newBalanceCmd(flags),
		a.newConfigCmd(flags),
		a.newVersionCmd(flags),
		a.newCompletionCmd(root),
	)
	return root
}

// newRuntime loads configuration and builds the printer for one command.
func (a *App) newRuntime(cmd *cobra.Command, flags *globalFlags) (*runtime, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.Usage, err)
	}
	if cfg.Defaults.Output == "json" && !cmd.Flags().Changed("json") {
		flags.json = true
	}
	rt := &runtime{
		app:    a,
		flags:  flags,
		cmd:    cmd,
		cfgDir: dir,
		cfg:    cfg,
		p:      output.New(a.Out, a.Err, a.In, output.Options{JSON: flags.json, Quiet: flags.quiet, NoColor: flags.noColor}),
	}
	if a.Interactive != nil {
		rt.p.SetInteractive(*a.Interactive)
	}
	if flags.debug {
		rt.debug = api.NewDebugLogger(a.Err)
		// Register the env key up front so it is redacted even in lines
		// logged before the first authenticated request.
		rt.debug.AddSecret(strings.TrimSpace(os.Getenv(auth.EnvAPIKey)))
	}
	return rt, nil
}

// baseURL returns the validated API origin for this invocation.
func (rt *runtime) baseURL() (string, error) {
	base, err := config.NormalizeBaseURL(os.Getenv(EnvBaseURL))
	if err != nil {
		return "", exitcode.Wrap(exitcode.Usage, fmt.Errorf("%s: %w", EnvBaseURL, err))
	}
	return base, nil
}

func (rt *runtime) consoleURL() (string, error) {
	base, err := rt.baseURL()
	if err != nil {
		return "", err
	}
	console, err := config.ConsoleURL(base, os.Getenv(EnvConsoleURL))
	if err != nil {
		return "", exitcode.Wrap(exitcode.Usage, fmt.Errorf("%s: %w", EnvConsoleURL, err))
	}
	return console, nil
}

func (rt *runtime) credentialStore() *auth.Store {
	if rt.store == nil {
		rt.store = rt.app.NewStore(rt.cfgDir)
	}
	return rt.store
}

func (rt *runtime) oauthClient(base string) *auth.OAuthClient {
	return &auth.OAuthClient{BaseURL: base, UserAgent: api.UserAgent(), Debugf: rt.debug.Logf}
}

// session resolves the credential bound to the current base URL.
func (rt *runtime) session() (*auth.Session, error) {
	base, err := rt.baseURL()
	if err != nil {
		return nil, err
	}
	return auth.Resolve(rt.credentialStore(), base, rt.oauthClient(base))
}

func (rt *runtime) newClient(base string, tokens api.TokenSource) *api.Client {
	c := api.New(base, tokens, rt.debug)
	if rt.app.RetryWait > 0 {
		c.SetRetryWait(rt.app.RetryWait)
	}
	return c
}

// client returns an authenticated API client, or exit code 3 when there is
// no credential for this host.
func (rt *runtime) client() (*api.Client, error) {
	sess, err := rt.session()
	if err != nil {
		return nil, err
	}
	return rt.newClient(sess.BaseURL, sess), nil
}

// publicClient returns a client that sends no credential.
func (rt *runtime) publicClient() (*api.Client, error) {
	base, err := rt.baseURL()
	if err != nil {
		return nil, err
	}
	return rt.newClient(base, nil), nil
}

// flagName extracts the flag a pflag error is about. Only names that are
// actually defined on the command are returned, so text the user typed is
// never taken for a flag name.
var flagNameInError = regexp.MustCompile(`(?:for "(?:-\w, )?--([a-z][a-z0-9-]*)" flag|flag needs an argument: --([a-z][a-z0-9-]*)|flag needs an argument: '(\w)' in)`)

// flagError rewrites a flag parsing error without the offending input.
// pflag quotes the value it could not parse and the unknown flag it saw; if
// that was a pasted secret (`--with-api-key=sk-…`, `-ksk-…`) it would be
// copied to the terminal and to CI logs.
func flagError(cmd *cobra.Command, err error) error {
	msg := err.Error()
	hint := fmt.Sprintf("\n\nRun `%s --help` for the flags it accepts.", cmd.CommandPath())
	switch {
	case strings.HasPrefix(msg, "unknown flag"), strings.HasPrefix(msg, "unknown shorthand flag"):
		if looksLikeCredentialFlag(msg) {
			return usagef("unknown flag. Credentials are never accepted as arguments: set %s, or run `apipod auth login`", auth.EnvAPIKey)
		}
		return usagef("unknown flag.%s", hint)
	case strings.HasPrefix(msg, "invalid argument"):
		if name := definedFlagName(cmd, msg); name != "" {
			return usagef("invalid value for --%s.%s", name, hint)
		}
		return usagef("invalid flag value.%s", hint)
	case strings.HasPrefix(msg, "flag needs an argument"):
		if name := definedFlagName(cmd, msg); name != "" {
			return usagef("--%s needs a value.%s", name, hint)
		}
		return usagef("a flag is missing its value.%s", hint)
	}
	return usagef("invalid flags.%s", hint)
}

// looksLikeCredentialFlag recognises attempts to pass a secret as a flag.
func looksLikeCredentialFlag(msg string) bool {
	for _, name := range []string{"--api-key", "--apikey", "--key", "--token", "--secret", "--password"} {
		if strings.HasPrefix(msg, "unknown flag: "+name) {
			return true
		}
	}
	return false
}

func definedFlagName(cmd *cobra.Command, msg string) string {
	m := flagNameInError.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	for _, candidate := range m[1:] {
		if candidate == "" {
			continue
		}
		if f := cmd.Flags().Lookup(candidate); f != nil {
			return f.Name
		}
		if len(candidate) == 1 {
			if f := cmd.Flags().ShorthandLookup(candidate); f != nil {
				return f.Name
			}
		}
	}
	return ""
}

// usagef builds an exit-code-2 error.
func usagef(format string, args ...any) error {
	return exitcode.New(exitcode.Usage, format, args...)
}

// noArgs rejects positional arguments without repeating them. cobra.NoArgs
// echoes the offending argument, which would copy a secret that was passed by
// mistake (`auth login --with-api-key sk-…`) into the terminal and CI logs.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if cmd.HasAvailableSubCommands() {
		names := []string{}
		for _, sub := range cmd.Commands() {
			if sub.IsAvailableCommand() {
				names = append(names, sub.Name())
			}
		}
		return fmt.Errorf("unknown subcommand; available: %s", strings.Join(names, ", "))
	}
	return fmt.Errorf("this command takes no arguments, received %d", len(args))
}

// positional wraps a cobra argument validator so its failures exit with code 2.
func positional(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := v(cmd, a); err != nil {
			return usagef("%v\n\nUsage: %s", err, cmd.UseLine())
		}
		return nil
	}
}

// run adapts a command body to cobra, building the runtime first.
func (a *App) run(flags *globalFlags, fn func(rt *runtime, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		rt, err := a.newRuntime(cmd, flags)
		if err != nil {
			return err
		}
		return fn(rt, args)
	}
}

// newHelpCmd replaces cobra's help command, which prints an unknown topic
// back to the terminal.
func (a *App) newHelpCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(_ *cobra.Command, args []string) error {
			target, rest, err := root.Find(args)
			if err != nil || target == nil || len(rest) > 0 {
				return usagef("unknown help topic. Run `apipod --help` for the list of commands")
			}
			return target.Help()
		},
	}
}
