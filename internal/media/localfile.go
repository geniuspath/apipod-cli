// Package media handles everything that crosses the local filesystem
// boundary: validating files before upload, turning local paths into URLs,
// and writing downloaded results to disk.
package media

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gabriel-vasile/mimetype"
)

// Media kinds, as reported by POST /v1/uploads.
const (
	KindImage = "image"
	KindAudio = "audio"
	KindVideo = "video"
)

const (
	mb = 1024 * 1024
	// sniffBytes is how much of a file is read to detect its type. It matches
	// the mimetype library's own header limit, so the CLI and the server
	// (which uses the same library) reach the same verdict.
	sniffBytes = 3072
	// MaxFilesPerCommand caps how many local files one command may upload.
	MaxFilesPerCommand = 20
)

// policy describes one accepted media type.
type policy struct {
	kind        string
	contentType string // canonical Content-Type sent to the server
	maxBytes    int64
	ext         string // extension used for downloaded results
}

// allowedMedia mirrors the server's upload allow-list
// (internal/handler/upload/media.go in the APIPod backend): the same sniffed
// MIME types and the same per-type size limits. Nothing outside this table
// ever leaves the machine.
var allowedMedia = map[string]policy{
	"image/png":       {KindImage, "image/png", 20 * mb, "png"},
	"image/jpeg":      {KindImage, "image/jpeg", 20 * mb, "jpg"},
	"image/webp":      {KindImage, "image/webp", 20 * mb, "webp"},
	"image/gif":       {KindImage, "image/gif", 20 * mb, "gif"},
	"audio/mpeg":      {KindAudio, "audio/mpeg", 50 * mb, "mp3"},
	"audio/wav":       {KindAudio, "audio/wav", 50 * mb, "wav"},
	"audio/x-m4a":     {KindAudio, "audio/mp4", 50 * mb, "m4a"},
	"video/mp4":       {KindVideo, "video/mp4", 100 * mb, "mp4"},
	"video/webm":      {KindVideo, "video/webm", 100 * mb, "webm"},
	"video/quicktime": {KindVideo, "video/quicktime", 100 * mb, "mov"},
}

// ErrUnsupportedMedia is returned for content outside the allow-list.
var ErrUnsupportedMedia = errors.New("not a supported media file (PNG, JPEG, WebP, GIF, MP3, WAV, M4A, MP4, WebM or MOV)")

// lookupPolicy walks up the detected type's ancestry so subtypes (M4V inside
// the MP4 family, for example) resolve to their allow-listed parent — the
// same walk the server does.
func lookupPolicy(header []byte) (policy, bool) {
	for mime := mimetype.Detect(header); mime != nil; mime = mime.Parent() {
		if p, ok := allowedMedia[mime.String()]; ok {
			return p, true
		}
	}
	return policy{}, false
}

// LocalFile is a local file that passed every pre-upload check.
type LocalFile struct {
	Path        string // as given by the user
	Name        string // basename only; the only name sent to the server
	Kind        string // image, audio or video
	ContentType string
	Size        int64

	info os.FileInfo
}

// Inspect validates a path the user named explicitly. It makes no network
// request and reads at most sniffBytes of the file.
//
// Rejected: directories, symbolic links, devices, FIFOs and sockets; empty
// files; anything whose content is not allow-listed media (so a private key
// or .env file is refused whatever its extension); files over the per-type
// size limit.
func Inspect(path string) (LocalFile, error) {
	if path == "" {
		return LocalFile{}, errors.New("empty file path")
	}
	// Lstat, not Stat: a symlink is judged as a symlink, never followed.
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalFile{}, fmt.Errorf("%s: no such file", path)
		}
		return LocalFile{}, fmt.Errorf("%s: %w", path, unwrapPathError(err))
	}
	switch mode := info.Mode(); {
	case mode.IsDir():
		return LocalFile{}, fmt.Errorf("%s: is a directory; name each file explicitly", path)
	case mode&os.ModeSymlink != 0:
		return LocalFile{}, fmt.Errorf("%s: is a symbolic link; pass the real file path", path)
	case !mode.IsRegular():
		return LocalFile{}, fmt.Errorf("%s: not a regular file", path)
	}
	if info.Size() == 0 {
		return LocalFile{}, fmt.Errorf("%s: file is empty", path)
	}

	f, err := openVerified(path, info)
	if err != nil {
		return LocalFile{}, err
	}
	defer f.Close()

	header := make([]byte, sniffBytes)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return LocalFile{}, fmt.Errorf("%s: %w", path, unwrapPathError(err))
	}
	p, ok := lookupPolicy(header[:n])
	if !ok {
		return LocalFile{}, fmt.Errorf("%s: %w", path, ErrUnsupportedMedia)
	}
	if info.Size() > p.maxBytes {
		return LocalFile{}, fmt.Errorf("%s: %s files must be at most %d MB (this one is %.1f MB)",
			path, p.kind, p.maxBytes/mb, float64(info.Size())/mb)
	}
	return LocalFile{
		Path:        path,
		Name:        filepath.Base(path),
		Kind:        p.kind,
		ContentType: p.contentType,
		Size:        info.Size(),
		info:        info,
	}, nil
}

// Open returns a reader over the inspected file. It refuses if the path now
// points at a different file than the one Inspect approved.
func (f LocalFile) Open() (io.ReadCloser, error) {
	file, err := openVerified(f.Path, f.info)
	if err != nil {
		return nil, err
	}
	// Cap the stream at the approved size: a file that grows after the check
	// cannot push more bytes than were validated.
	return &limitedFile{Reader: io.LimitReader(file, f.Size), Closer: file}, nil
}

type limitedFile struct {
	io.Reader
	io.Closer
}

// openVerified opens path and confirms the opened file is the very file that
// was Lstat'ed. This closes the check-then-open race: if the path was swapped
// for a symlink in between, the identities differ and the open is refused.
func openVerified(path string, want os.FileInfo) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, unwrapPathError(err))
	}
	got, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, unwrapPathError(err))
	}
	if !got.Mode().IsRegular() || !os.SameFile(want, got) {
		f.Close()
		return nil, fmt.Errorf("%s: file changed while it was being checked", path)
	}
	return f, nil
}

// unwrapPathError drops the path that *os.PathError repeats; callers prefix
// the path themselves.
func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
