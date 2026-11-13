package cli

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/api"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/media"
)

// generateOptions are the flags of `apipod image` and `apipod video`.
type generateOptions struct {
	kind       string
	model      string
	images     []string
	videos     []string
	audios     []string
	aspect     string
	n          int
	duration   int
	resolution string
	extras     []string
	wait       waitOptions
}

func (a *App) newGenerateCmd(flags *globalFlags, kind string) *cobra.Command {
	opts := &generateOptions{kind: kind}
	cmd := &cobra.Command{RunE: a.run(flags, func(rt *runtime, args []string) error { return rt.generate(opts, args) })}

	f := cmd.Flags()
	f.StringVarP(&opts.model, "model", "m", "", "model id (see `apipod models list`)")
	f.StringArrayVar(&opts.images, "image", nil, "reference image: local file or URL (repeatable)")
	f.StringVar(&opts.aspect, "aspect", "", "aspect ratio, e.g. 16:9, 9:16, 1:1")
	f.StringArrayVarP(&opts.extras, "extra", "x", nil, "model-specific parameter as key=value; key=@file uploads a local file (repeatable)")
	opts.wait.register(cmd)

	const common = `
The command estimates the cost first. An estimate above spend.confirm_above
(default $1) needs confirmation: answer the prompt, pass --yes, or set a cap
with --max-cost. Without a terminal and without either flag the command
refuses to spend and exits with code 6.

Local files given as references are uploaded first; their URLs are public.
Model-specific parameters ("apipod models params <model>") go in -x key=value;
values that parse as JSON are sent as JSON (-x seed=42, -x draft=true).
The reference flags fill image_urls, video_urls and audio_urls. A model that
names its input differently takes it through -x, where @ uploads a local file:
-x video_url=@./clip.mp4

By default the command waits for the job and downloads the results into the
directory given with -o (default: the current directory). With --no-wait it
prints the job id and returns immediately.`

	if kind == api.KindImage {
		cmd.Use = "image <prompt>"
		cmd.Short = "Generate or edit an image"
		cmd.Long = "Generate an image from a prompt, or edit reference images.\n" + common
		cmd.Example = `  apipod image "a red fox in the snow" -m gpt-image-2 --aspect 16:9
  apipod image "make it night" -m gpt-image-2 --image ./photo.png -o ./out
  apipod image "a logo" -m gpt-image-2 -n 2 --max-cost 0.50 --json`
		cmd.Args = positional(cobra.ExactArgs(1))
		f.IntVarP(&opts.n, "count", "n", 0, "number of images")
		return cmd
	}

	cmd.Use = "video [prompt]"
	cmd.Short = "Generate a video"
	cmd.Long = "Generate a video from a prompt and/or reference media.\n" + common
	cmd.Example = `  apipod video "a drone shot over a fjord" -m kling-3.0-turbo-t2v --duration 5
  apipod video "she turns and smiles" -m <model> --image ./first-frame.png --yes
  apipod video -m <model> --image ./frame.png --no-wait --max-cost 2 --json`
	cmd.Args = positional(cobra.MaximumNArgs(1))
	f.StringArrayVar(&opts.videos, "video", nil, "reference video: local file or URL (repeatable)")
	f.StringArrayVar(&opts.audios, "audio", nil, "reference audio: local file or URL (repeatable)")
	f.IntVar(&opts.duration, "duration", 0, "duration in seconds")
	f.StringVar(&opts.resolution, "resolution", "", "resolution, e.g. 720p, 1080p")
	return cmd
}

// mediaField is one request field filled from media references.
type mediaField struct {
	key  string
	refs []media.Ref
	// list is false when the field takes a single URL instead of an array.
	list bool
}

// parseRefs validates every reference of a flag. Local files are fully
// checked here; nothing has been sent yet.
func parseRefs(values []string, wantKind string) ([]media.Ref, error) {
	refs := make([]media.Ref, 0, len(values))
	for _, v := range values {
		ref, err := media.ParseRef(v, wantKind)
		if err != nil {
			return nil, exitcode.Wrap(exitcode.Validation, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// collectMedia gathers every media reference of the command — dedicated
// flags and -x key=@file — validating all local files up front.
func collectMedia(opts *generateOptions, ex extras) ([]mediaField, error) {
	flags := []struct {
		key    string
		kind   string
		values []string
	}{
		{"image_urls", media.KindImage, opts.images},
		{"video_urls", media.KindVideo, opts.videos},
		{"audio_urls", media.KindAudio, opts.audios},
	}
	// Usage errors first: they need no filesystem access.
	for _, flag := range flags {
		if len(flag.values) == 0 {
			continue
		}
		if _, clash := ex.files[flag.key]; clash {
			return nil, usagef("-x %s=@file conflicts with the dedicated flag for %s", flag.key, flag.key)
		}
		if _, clash := ex.values[flag.key]; clash {
			return nil, usagef("-x %s conflicts with the dedicated flag for %s", flag.key, flag.key)
		}
	}

	var fields []mediaField
	for _, flag := range flags {
		if len(flag.values) == 0 {
			continue
		}
		refs, err := parseRefs(flag.values, flag.kind)
		if err != nil {
			return nil, err
		}
		fields = append(fields, mediaField{key: flag.key, refs: refs, list: true})
	}
	for _, key := range ex.order {
		// -x files may be any media kind; the model decides what it accepts.
		refs, err := parseRefs(ex.files[key], "")
		if err != nil {
			return nil, err
		}
		fields = append(fields, mediaField{key: key, refs: refs, list: len(refs) > 1 || listKey(key)})
	}

	local := 0
	for _, f := range fields {
		local += media.CountLocal(f.refs)
	}
	if local > media.MaxFilesPerCommand {
		return nil, exitcode.New(exitcode.Validation, "too many local files: at most %d can be uploaded per command", media.MaxFilesPerCommand)
	}
	return fields, nil
}

// hasVideoInput reports whether the request supplies a reference video,
// which some models bill extra. The server counts video_urls, video_url and
// reference_videos when it bills; any other parameter whose name mentions
// "video" and whose value is a URL is counted as well. Erring towards "yes"
// can only make the estimate higher, never lower than the charge.
func hasVideoInput(fields []mediaField, body map[string]any) bool {
	for _, f := range fields {
		if strings.Contains(f.key, "video") && len(f.refs) > 0 {
			return true
		}
		for _, r := range f.refs {
			if r.File != nil && r.File.Kind == media.KindVideo {
				return true
			}
		}
	}
	for key, value := range body {
		if strings.Contains(key, "video") && carriesURL(value) {
			return true
		}
	}
	return false
}

// carriesURL reports whether v is a URL reference, or a list containing
// one.
func carriesURL(v any) bool {
	switch x := v.(type) {
	case string:
		return media.IsRemote(strings.TrimSpace(x))
	case []any:
		for _, item := range x {
			if carriesURL(item) {
				return true
			}
		}
	}
	return false
}

// generationPlan is a generation request that passed local validation.
type generationPlan struct {
	model   string
	body    map[string]any
	fields  []mediaField
	maxCost *float64
}

// planGeneration validates the command line and builds the request body. It
// touches the local filesystem only: no network request is made here, so a
// rejected flag or file means nothing has been sent.
func (rt *runtime) planGeneration(opts *generateOptions, args []string) (generationPlan, error) {
	model := strings.TrimSpace(opts.model)
	if model == "" {
		if opts.kind == api.KindImage {
			model = rt.cfg.Defaults.ImageModel
		} else {
			model = rt.cfg.Defaults.VideoModel
		}
	}
	if model == "" {
		return generationPlan{}, usagef("no model given: pass -m <model>, or set a default with `apipod config set defaults.%s_model <model>`", opts.kind)
	}
	prompt := ""
	if len(args) == 1 {
		prompt = strings.TrimSpace(args[0])
	}
	if opts.kind == api.KindImage && prompt == "" {
		return generationPlan{}, usagef("the prompt must not be empty")
	}
	ex, err := parseExtras(opts.extras)
	if err != nil {
		return generationPlan{}, err
	}
	fields, err := collectMedia(opts, ex)
	if err != nil {
		return generationPlan{}, err
	}
	plan := generationPlan{model: model, fields: fields, body: requestBody(opts, ex, model, prompt)}
	if rt.cmd.Flags().Changed("max-cost") {
		// NaN compares false against everything: it would pass a plain
		// "negative?" check and then never be exceeded by any estimate.
		if v := rt.flags.maxCost; math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return generationPlan{}, usagef("--max-cost must be a non-negative number of USD")
		}
		plan.maxCost = &rt.flags.maxCost
	}
	return plan, nil
}

// requestBody merges -x parameters with the dedicated flags. Dedicated flags
// win over -x, as in the MCP tools.
func requestBody(opts *generateOptions, ex extras, model, prompt string) map[string]any {
	body := make(map[string]any, len(ex.values)+8)
	for k, v := range ex.values {
		body[k] = v
	}
	body["model"] = model
	if prompt != "" {
		body["prompt"] = prompt
	}
	if opts.aspect != "" {
		body["aspect_ratio"] = opts.aspect
	}
	if opts.n > 0 {
		body["n"] = opts.n
	}
	if opts.duration > 0 {
		body["duration"] = opts.duration
	}
	if opts.resolution != "" {
		body["resolution"] = opts.resolution
	}
	// The CLI polls for the result; a callback URL would make the server
	// push task data to an address chosen on the command line.
	delete(body, "callback_url")
	return body
}

// uploadReferences uploads the plan's local files and returns the request
// body with every media field set to URLs.
func (rt *runtime) uploadReferences(ctx context.Context, client *api.Client, plan generationPlan) (map[string]any, error) {
	body := make(map[string]any, len(plan.body)+len(plan.fields))
	for k, v := range plan.body {
		body[k] = v
	}
	for _, f := range plan.fields {
		if media.CountLocal(f.refs) > 0 {
			rt.noticePublicUploads()
			break
		}
	}
	upload := rt.uploader(client)
	for _, f := range plan.fields {
		urls, err := media.ResolveAll(ctx, f.refs, upload)
		if err != nil {
			return nil, err
		}
		if f.list {
			body[f.key] = urls
		} else {
			body[f.key] = urls[0]
		}
	}
	return body, nil
}

// generate runs `apipod image` / `apipod video`. The order is the safety
// property: validate locally, resolve the credential, estimate and pass the
// spending guard — and only then upload anything or create a task.
func (rt *runtime) generate(opts *generateOptions, args []string) error {
	plan, err := rt.planGeneration(opts, args)
	if err != nil {
		return err
	}
	client, err := rt.client()
	if err != nil {
		return err
	}
	ctx := rt.cmd.Context()

	estimate, err := rt.estimate(client, plan.model, plan.body, hasVideoInput(plan.fields, plan.body))
	if err != nil {
		return err
	}
	if estimate != nil {
		rt.p.Infof("Estimated cost: %s", formatUSD(*estimate))
	}
	if err := guardSpend(ctx, rt.p, rt.app.In, spendCheck{
		estimate:  estimate,
		threshold: rt.cfg.ConfirmThreshold(),
		maxCost:   plan.maxCost,
		yes:       rt.flags.yes,
	}); err != nil {
		return err
	}

	body, err := rt.uploadReferences(ctx, client, plan)
	if err != nil {
		return err
	}
	task, err := client.Generate(ctx, opts.kind, body)
	if err != nil {
		return err
	}
	if task.TaskID == "" {
		return errors.New("the server accepted the request but returned no job id")
	}
	job := api.Job{JobID: task.TaskID, Kind: opts.kind, Status: task.Status}
	if rt.flags.noWait {
		rt.p.Infof("Job submitted. Check it with `apipod job wait %s`.", job.JobID)
		return rt.printJob(job, estimate)
	}
	return rt.finishJob(client, job, opts.wait, estimate)
}

// estimate quotes the request. A nil estimate with a nil error means the
// price could not be determined; the spending guard then treats the request
// as needing explicit approval. Errors that would also fail the generation
// (bad credential, unknown model) are returned.
func (rt *runtime) estimate(client *api.Client, model string, body map[string]any, hasVideo bool) (*float64, error) {
	est, err := client.EstimateCost(rt.cmd.Context(), api.EstimateRequest{
		ModelID:          model,
		Dimensions:       pricingDimensions(body),
		HasVideoMaterial: hasVideo,
	})
	if err == nil {
		return &est.Price, nil
	}
	if rt.cmd.Context().Err() != nil {
		return nil, rt.cmd.Context().Err()
	}
	switch exitcode.From(err) {
	case exitcode.Auth:
		return nil, err
	case exitcode.NotFound:
		return nil, exitcode.New(exitcode.NotFound, "model %q not found. List models with `apipod models list`", model)
	}
	rt.p.Warnf("could not estimate the cost: %v", err)
	return nil, nil
}
