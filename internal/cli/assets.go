package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/api"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/media"
)

// assetTypes maps the --type value to the server's asset type.
var assetTypes = map[string]string{
	media.KindImage: "Image",
	media.KindVideo: "Video",
	media.KindAudio: "Audio",
}

const assetPollInterval = 3 * time.Second

func (a *App) newAssetsCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assets",
		Short: "Manage reviewed reference assets",
		Long: `Manage the asset library: reference media that has to pass review before a
model accepts it (for example real-person material for Seedance).

An asset is referenced in generation parameters as assetId://<id>, e.g.
  apipod video "..." -m <model> -x image_urls='["assetId://<id>"]'`,
		Args: positional(noArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	var (
		assetType, name string
		wait            bool
		timeout         time.Duration
	)
	create := &cobra.Command{
		Use:   "create <file|url>",
		Short: "Submit a file or URL for review",
		Long: `Submit reference media for review.

A local file is uploaded first (its URL is public) and its type is detected
from its content, so --type is only required for a URL. With --wait the
command polls until the review ends: exit code 0 when the asset is ACTIVE,
8 when it is rejected.`,
		Example: `  apipod assets create ./face.jpg --name "lead actor" --wait
  apipod assets create https://example.com/clip.mp4 --type video`,
		Args: positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error {
			return rt.createAsset(args[0], assetType, name, wait, timeout)
		}),
	}
	create.Flags().StringVar(&assetType, "type", "", "asset type: image, video or audio (detected for local files)")
	create.Flags().StringVar(&name, "name", "", "display name")
	create.Flags().BoolVar(&wait, "wait", false, "wait until the review ends")
	create.Flags().DurationVar(&timeout, "timeout", defaultWaitTimeout, "how long --wait waits before giving up (exit code 7)")

	var params api.AssetListParams
	list := &cobra.Command{
		Use:   "list",
		Short: "List assets",
		Args:  positional(noArgs),
		RunE: a.run(flags, func(rt *runtime, _ []string) error {
			if params.Type != "" {
				if _, ok := assetTypes[strings.ToLower(params.Type)]; !ok {
					return usagef("--type must be one of: image, video, audio")
				}
				params.Type = assetTypes[strings.ToLower(params.Type)]
			}
			return rt.listAssets(params)
		}),
	}
	list.Flags().StringVar(&params.Type, "type", "", "filter by type: image, video or audio")
	list.Flags().StringVar(&params.Status, "status", "", "filter by status, e.g. ACTIVE, PROCESSING, FAILED")
	list.Flags().StringVar(&params.Search, "search", "", "filter by name")
	list.Flags().IntVar(&params.Page, "page", 1, "page number")
	list.Flags().IntVar(&params.PageSize, "page-size", 20, "items per page")

	get := &cobra.Command{
		Use:   "get <asset-id>",
		Short: "Show one asset",
		Args:  positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error {
			client, err := rt.client()
			if err != nil {
				return err
			}
			asset, err := client.GetAsset(rt.cmd.Context(), args[0])
			if err != nil {
				return assetNotFound(args[0], err)
			}
			return rt.printAsset(asset)
		}),
	}

	del := &cobra.Command{
		Use:   "delete <asset-id>",
		Short: "Delete one asset",
		Args:  positional(cobra.ExactArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error {
			client, err := rt.client()
			if err != nil {
				return err
			}
			id := api.AssetID(args[0])
			if err := client.DeleteAsset(rt.cmd.Context(), id); err != nil {
				return assetNotFound(args[0], err)
			}
			return rt.p.Result(map[string]any{"assetId": id, "deleted": true}, func(w io.Writer) {
				fmt.Fprintf(w, "Deleted asset %s.\n", id)
			})
		}),
	}

	cmd.AddCommand(create, list, get, del)
	return cmd
}

func assetNotFound(id string, err error) error {
	if api.IsNotFound(err) {
		return exitcode.New(exitcode.NotFound, "asset %q not found", api.AssetID(id))
	}
	return err
}

func (rt *runtime) createAsset(source, typeFlag, name string, wait bool, timeout time.Duration) error {
	// ---- Local validation: nothing is sent before this block passes ----
	kind := strings.ToLower(strings.TrimSpace(typeFlag))
	if kind != "" {
		if _, ok := assetTypes[kind]; !ok {
			return usagef("--type must be one of: image, video, audio")
		}
	}
	if strings.HasPrefix(source, "assetId://") {
		return usagef("the source is already an asset reference")
	}
	ref, err := media.ParseRef(source, kind)
	if err != nil {
		return exitcode.Wrap(exitcode.Validation, err)
	}
	if ref.File != nil {
		kind = ref.File.Kind
	}
	if kind == "" {
		return usagef("--type is required when the source is a URL (image, video or audio)")
	}

	client, err := rt.client()
	if err != nil {
		return err
	}
	ctx := rt.cmd.Context()
	if ref.File != nil {
		rt.noticePublicUploads()
	}
	urls, err := media.ResolveAll(ctx, []media.Ref{ref}, rt.uploader(client))
	if err != nil {
		return err
	}
	asset, err := client.CreateAsset(ctx, api.CreateAssetRequest{AssetType: assetTypes[kind], URL: urls[0], Name: name})
	if err != nil {
		return err
	}
	if !wait {
		return rt.printAsset(asset)
	}

	asset, err = rt.waitForAsset(client, asset, timeout)
	if err != nil {
		return err
	}
	if err := rt.printAsset(asset); err != nil {
		return err
	}
	if asset.Status != api.AssetStatusActive {
		reason := "no reason given"
		if asset.ErrorMessage != nil && *asset.ErrorMessage != "" {
			reason = *asset.ErrorMessage
		}
		return exitcode.New(exitcode.Validation, "asset %s was not approved (%s): %s", asset.AssetID, asset.Status, reason)
	}
	return nil
}

// assetSettled reports whether the review has ended. Anything that is not an
// in-progress state counts, so an unexpected terminal status cannot make
// --wait spin until the timeout.
func assetSettled(status string) bool {
	switch strings.ToUpper(status) {
	case "", "NONE", "UPLOADING", "PROCESSING":
		return false
	}
	return true
}

func (rt *runtime) waitForAsset(client *api.Client, asset api.Asset, timeout time.Duration) (api.Asset, error) {
	if assetSettled(asset.Status) {
		return asset, nil
	}
	ctx, cancel := context.WithTimeout(rt.cmd.Context(), timeout)
	defer cancel()
	interval := rt.app.PollInterval
	if interval <= 0 {
		interval = assetPollInterval
	}
	rt.p.Infof("Waiting for the review of asset %s ...", asset.AssetID)
	for !assetSettled(asset.Status) {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return asset, exitcode.New(exitcode.Timeout,
					"timed out after %s; asset %s is still under review. Check it with `apipod assets get %s`", timeout, asset.AssetID, asset.AssetID)
			}
			return asset, ctx.Err()
		case <-time.After(interval):
		}
		next, err := client.GetAsset(ctx, asset.AssetID)
		if err != nil {
			if ctx.Err() != nil {
				continue // reported by the select above
			}
			return asset, fmt.Errorf("checking asset %s failed: %w", asset.AssetID, err)
		}
		asset = next
	}
	return asset, nil
}

func (rt *runtime) printAsset(asset api.Asset) error {
	return rt.p.Result(asset, func(w io.Writer) {
		pairs := []string{"Asset", asset.AssetID, "Reference", asset.Ref, "Type", asset.AssetType, "Status", asset.Status}
		if asset.Name != "" {
			pairs = append(pairs, "Name", asset.Name)
		}
		if asset.ErrorMessage != nil && *asset.ErrorMessage != "" {
			pairs = append(pairs, "Error", *asset.ErrorMessage)
		}
		pairs = append(pairs, "URL", asset.URL)
		rt.p.KV(w, pairs...)
	})
}

func (rt *runtime) listAssets(params api.AssetListParams) error {
	client, err := rt.client()
	if err != nil {
		return err
	}
	page, err := client.ListAssets(rt.cmd.Context(), params)
	if err != nil {
		return err
	}
	if page.Items == nil {
		page.Items = []api.Asset{}
	}
	return rt.p.Result(page, func(w io.Writer) {
		if len(page.Items) == 0 {
			fmt.Fprintln(w, "No assets found.")
			return
		}
		rows := make([][]string, len(page.Items))
		for i, a := range page.Items {
			rows[i] = []string{a.Ref, a.AssetType, a.Status, a.Name}
		}
		rt.p.Table(w, []string{"REFERENCE", "TYPE", "STATUS", "NAME"}, rows)
		if page.TotalPages > 1 {
			fmt.Fprintf(w, "\nPage %d of %d (%d assets)\n", page.Page, page.TotalPages, page.Total)
		}
	})
}
