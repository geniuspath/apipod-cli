package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/geniuspath/apipod-cli/internal/api"
	"github.com/geniuspath/apipod-cli/internal/config"
	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/media"
)

// uploadedFile is one entry of `apipod upload --json`.
type uploadedFile struct {
	File        string `json:"file"`
	URL         string `json:"url"`
	Type        string `json:"type"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
}

func (a *App) newUploadCmd(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "upload <file>...",
		Short: "Upload local media files and print their URLs",
		Long: fmt.Sprintf(`Upload local images, videos or audio files and print a URL for each.

The URLs are PUBLIC: anyone who has a URL can open the file. Do not upload
anything you would not publish.

Only files named explicitly are uploaded, at most %d per command. Directories,
symbolic links and special files are refused, and so is any file whose content
is not PNG, JPEG, WebP, GIF, MP3, WAV, M4A, MP4, WebM or MOV — whatever its
extension. A refused file stops the command before anything is sent.

Size limits: images 20 MB, audio 50 MB, video 100 MB.`, media.MaxFilesPerCommand),
		Example: `  apipod upload ./photo.png ./clip.mp4
  apipod upload ./photo.png --json | jq -r '.[0].url'`,
		Args: positional(cobra.MinimumNArgs(1)),
		RunE: a.run(flags, func(rt *runtime, args []string) error { return rt.upload(args) }),
	}
}

// inspectFiles validates every path before anything is sent. One bad path
// rejects the whole command, so nothing is uploaded on a partial match.
func inspectFiles(paths []string) ([]media.LocalFile, error) {
	if len(paths) > media.MaxFilesPerCommand {
		return nil, exitcode.New(exitcode.Validation, "too many files: at most %d can be uploaded per command", media.MaxFilesPerCommand)
	}
	files := make([]media.LocalFile, 0, len(paths))
	for _, path := range paths {
		if media.IsRemote(path) {
			return nil, exitcode.New(exitcode.Validation, "%s: already a URL; upload takes local files", path)
		}
		file, err := media.Inspect(path)
		if err != nil {
			return nil, exitcode.Wrap(exitcode.Validation, err)
		}
		files = append(files, file)
	}
	return files, nil
}

func (rt *runtime) upload(paths []string) error {
	files, err := inspectFiles(paths)
	if err != nil {
		return err
	}
	client, err := rt.client()
	if err != nil {
		return err
	}
	rt.noticePublicUploads()

	ctx := rt.cmd.Context()
	results := make([]uploadedFile, 0, len(files))
	for _, file := range files {
		up, err := rt.uploadOne(ctx, client, file)
		if err != nil {
			return fmt.Errorf("upload %s: %w", file.Path, err)
		}
		results = append(results, uploadedFile{File: file.Path, URL: up.URL, Type: up.Type, ContentType: up.ContentType, Bytes: up.Bytes})
	}
	return rt.p.Result(results, func(w io.Writer) {
		rows := make([][]string, len(results))
		for i, r := range results {
			rows[i] = []string{r.File, r.Type, fmt.Sprintf("%.1f MB", float64(r.Bytes)/(1024*1024)), r.URL}
		}
		rt.p.Table(w, []string{"FILE", "TYPE", "SIZE", "URL"}, rows)
	})
}

// uploadOne sends one validated file. Only its basename is sent.
func (rt *runtime) uploadOne(ctx context.Context, client *api.Client, file media.LocalFile) (api.Upload, error) {
	rt.p.Infof("Uploading %s (%.1f MB) ...", file.Path, float64(file.Size)/(1024*1024))
	up, err := client.UploadFile(ctx, file.Name, file.ContentType, file.Size, file.Open)
	if err != nil {
		return api.Upload{}, err
	}
	if up.URL == "" {
		return api.Upload{}, fmt.Errorf("the server returned no URL")
	}
	if up.Type == "" {
		up.Type = file.Kind
	}
	if up.Bytes == 0 {
		up.Bytes = file.Size
	}
	return up, nil
}

// uploader adapts uploadOne to media.Uploader for reference files.
func (rt *runtime) uploader(client *api.Client) media.Uploader {
	return func(ctx context.Context, file media.LocalFile) (string, error) {
		up, err := rt.uploadOne(ctx, client, file)
		return up.URL, err
	}
}

// noticePublicUploads explains, once, that uploaded files get public URLs.
// It is remembered only after an interactive run, so the notice is not used
// up by a script before a person has had the chance to read it.
func (rt *runtime) noticePublicUploads() {
	if rt.cfg.Notices.UploadPublic {
		return
	}
	rt.p.Infof("Note: uploaded files are stored at public URLs. Anyone with a URL can open the file.")
	if !rt.p.Interactive() {
		return
	}
	next := rt.cfg
	next.Notices.UploadPublic = true
	if err := config.Save(rt.cfgDir, next); err == nil {
		rt.cfg = next
	}
}
