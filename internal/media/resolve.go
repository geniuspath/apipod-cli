package media

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Ref is one media reference given on the command line: either something the
// server can already fetch (a URL, or a server-side reference scheme) or
// a local file.
type Ref struct {
	// Remote is the value to send as is; empty for a local file.
	Remote string
	// File is the validated local file; nil for a remote reference.
	File *LocalFile
}

// IsRemote reports whether s is passed to the server unchanged.
func IsRemote(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(s, "assetId://")
}

// ParseRef classifies one reference. wantKind ("" for any) restricts what a
// local file may be, so `--image clip.mp4` fails locally instead of at the
// server. A local file is fully validated here; nothing is uploaded yet.
func ParseRef(s, wantKind string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("empty media reference")
	}
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		return Ref{}, fmt.Errorf("inline data: URIs are not supported; pass a file path or an https URL")
	}
	if IsRemote(s) {
		if !strings.HasPrefix(s, "assetId://") {
			u, err := url.Parse(s)
			if err != nil || u.Host == "" {
				return Ref{}, fmt.Errorf("%s: not a valid URL", s)
			}
		}
		return Ref{Remote: s}, nil
	}
	file, err := Inspect(s)
	if err != nil {
		return Ref{}, err
	}
	if wantKind != "" && file.Kind != wantKind {
		return Ref{}, fmt.Errorf("%s: expected %s file, found %s", s, article(wantKind), file.Kind)
	}
	return Ref{File: &file}, nil
}

func article(kind string) string {
	if kind == KindImage || kind == KindAudio {
		return "an " + kind
	}
	return "a " + kind
}

// CountLocal returns how many refs are local files.
func CountLocal(refs []Ref) int {
	n := 0
	for _, r := range refs {
		if r.File != nil {
			n++
		}
	}
	return n
}

// Uploader uploads one validated file and returns its public URL.
type Uploader func(ctx context.Context, file LocalFile) (string, error)

// ResolveAll turns refs into URLs, uploading local files in order. Call it
// only after every ref of the command has been parsed, so a rejected file
// means no request is made at all.
func ResolveAll(ctx context.Context, refs []Ref, upload Uploader) ([]string, error) {
	urls := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.File == nil {
			urls = append(urls, r.Remote)
			continue
		}
		u, err := upload(ctx, *r.File)
		if err != nil {
			return nil, fmt.Errorf("upload %s: %w", r.File.Path, err)
		}
		urls = append(urls, u)
	}
	return urls, nil
}
