package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/api"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/media"
)

// defaultWaitTimeout bounds how long a command waits for a job.
const defaultWaitTimeout = 20 * time.Minute

// jobError is a failure that concerns a known job. The job id is repeated in
// the JSON error so an agent can resume with `apipod job wait`.
type jobError struct {
	JobID string
	Code  int
	Err   error
}

func (e *jobError) Error() string { return e.Err.Error() }
func (e *jobError) Unwrap() error { return e.Err }
func (e *jobError) ExitCode() int { return e.Code }

// waitOptions are the flags shared by commands that wait for a job.
type waitOptions struct {
	timeout time.Duration
	force   bool
}

func (o *waitOptions) register(cmd *cobra.Command) {
	cmd.Flags().DurationVar(&o.timeout, "timeout", defaultWaitTimeout, "how long to wait for the job before giving up (exit code 7)")
	cmd.Flags().BoolVar(&o.force, "force", false, "overwrite existing files when downloading results")
}

func (a *App) newJobCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Check or wait for a generation job",
		Args:  positional(noArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	get := &cobra.Command{
		Use:   "get <job-id>",
		Short: "Show the current status of a job",
		Args:  positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error {
			client, err := rt.client()
			if err != nil {
				return err
			}
			job, err := client.GetJob(rt.cmd.Context(), args[0])
			if err != nil {
				return jobNotFound(args[0], err)
			}
			return rt.printJob(job, nil)
		}),
	}

	var opts waitOptions
	wait := &cobra.Command{
		Use:   "wait <job-id>",
		Short: "Wait for a job to finish and download its results",
		Long: `Poll a job until it completes or fails, then download the results into the
directory given with -o (default: the current directory).

Exit code 7 means the wait timed out; the job keeps running and can be waited
for again.`,
		Args: positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error {
			client, err := rt.client()
			if err != nil {
				return err
			}
			// The first lookup settles whether this is a video or an image job.
			job, err := client.GetJob(rt.cmd.Context(), args[0])
			if err != nil {
				return jobNotFound(args[0], err)
			}
			return rt.finishJob(client, job, opts, nil)
		}),
	}
	opts.register(wait)

	cmd.AddCommand(get, wait)
	return cmd
}

func jobNotFound(id string, err error) error {
	if api.IsNotFound(err) {
		return exitcode.New(exitcode.NotFound, "job %q not found", id)
	}
	return err
}

func pollInterval(kind string) time.Duration {
	if kind == api.KindImage {
		return 3 * time.Second
	}
	return 5 * time.Second
}

// waitForJob polls until the job is terminal or the timeout passes.
func (rt *runtime) waitForJob(client *api.Client, job api.Job, timeout time.Duration) (api.Job, error) {
	if job.Done() {
		return job, nil
	}
	ctx, cancel := context.WithTimeout(rt.cmd.Context(), timeout)
	defer cancel()

	interval := rt.app.PollInterval
	if interval <= 0 {
		interval = pollInterval(job.Kind)
	}
	id, kind := job.JobID, job.Kind
	lastStatus := ""
	rt.p.Infof("Waiting for job %s ...", id)
	for {
		if job.Status != lastStatus {
			lastStatus = job.Status
			if job.Progress != nil {
				rt.p.Infof("  %s (%d%%)", job.Status, *job.Progress)
			} else {
				rt.p.Infof("  %s", job.Status)
			}
		}
		if job.Done() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return job, rt.waitInterrupted(ctx, id, timeout)
		case <-time.After(interval):
		}
		next, err := client.GetJobOfKind(ctx, kind, id)
		if err != nil {
			if ctx.Err() != nil {
				return job, rt.waitInterrupted(ctx, id, timeout)
			}
			return job, &jobError{JobID: id, Code: exitcode.From(err), Err: fmt.Errorf("checking job %s failed: %w", id, err)}
		}
		job = next
	}
}

func (rt *runtime) waitInterrupted(ctx context.Context, id string, timeout time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &jobError{JobID: id, Code: exitcode.Timeout,
			Err: fmt.Errorf("timed out after %s; job %s is still running. Resume with `apipod job wait %s`", timeout, id, id)}
	}
	rt.p.Infof("Job %s is still running. Resume with `apipod job wait %s`.", id, id)
	return ctx.Err()
}

// finishJob waits for a job, downloads its results and prints it. A failed
// job is printed and then reported as an error, so the exit code is non-zero.
func (rt *runtime) finishJob(client *api.Client, job api.Job, opts waitOptions, estimate *float64) error {
	job, err := rt.waitForJob(client, job, opts.timeout)
	if err != nil {
		return err
	}
	if job.Status == api.StatusFailed {
		if err := rt.printJob(job, estimate); err != nil {
			return err
		}
		reason := job.ErrorMessage
		if reason == "" {
			reason = "no reason given"
		}
		return &jobError{JobID: job.JobID, Code: exitcode.General,
			Err: fmt.Errorf("job %s failed: %s (a failed job is refunded automatically)", job.JobID, reason)}
	}

	job, downloadErr := rt.downloadResults(job, opts.force)
	if err := rt.printJob(job, estimate); err != nil {
		return err
	}
	if downloadErr != nil {
		return &jobError{JobID: job.JobID, Code: exitcode.General,
			Err: fmt.Errorf("the job completed, but a result could not be saved: %w", downloadErr)}
	}
	return nil
}

// downloadResults saves each result into the output directory and records
// the local path. It returns the first failure after trying every result.
func (rt *runtime) downloadResults(job api.Job, force bool) (api.Job, error) {
	if len(job.Results) == 0 {
		return job, nil
	}
	dir := rt.flags.output
	if dir == "" {
		dir = "."
	}
	var d *media.Downloader
	if rt.app.DownloadClient != nil {
		d = media.NewDownloaderWithClient(rt.app.DownloadClient)
	} else {
		d = media.NewDownloader()
	}
	d.Force = force

	ctx, cancel := context.WithTimeout(rt.cmd.Context(), media.DownloadTimeout)
	defer cancel()

	results := make([]api.JobAsset, len(job.Results))
	var firstErr error
	for i, asset := range job.Results {
		results[i] = asset
		path, err := d.Save(ctx, asset.URL, dir, job.JobID, i+1)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results[i].Path = path
	}
	job.Results = results
	return job, firstErr
}

// jobView is the printed form of a job.
type jobView struct {
	api.Job
	EstimatedUSD *float64 `json:"estimated_usd,omitempty"`
}

func (rt *runtime) printJob(job api.Job, estimate *float64) error {
	return rt.p.Result(jobView{Job: job, EstimatedUSD: estimate}, func(w io.Writer) {
		pairs := []string{"Job", job.JobID, "Status", job.Status}
		if job.Progress != nil && !job.Done() {
			pairs = append(pairs, "Progress", fmt.Sprintf("%d%%", *job.Progress))
		}
		if job.CostUSD != nil {
			pairs = append(pairs, "Cost", formatUSD(*job.CostUSD))
		} else if estimate != nil {
			pairs = append(pairs, "Estimated cost", formatUSD(*estimate))
		}
		if job.ErrorMessage != "" {
			pairs = append(pairs, "Error", job.ErrorMessage)
		}
		if job.Text != "" {
			pairs = append(pairs, "Text", job.Text)
		}
		for _, r := range job.Results {
			if r.Path != "" {
				pairs = append(pairs, "Saved", r.Path)
			} else {
				pairs = append(pairs, "Result", r.URL)
			}
		}
		rt.p.KV(w, pairs...)
	})
}
