package media

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/geniuspath/apipod-cli/internal/testutil"
)

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectAcceptsAllowListedMedia(t *testing.T) {
	cases := []struct {
		name        string
		data        []byte
		kind        string
		contentType string
	}{
		{"a.png", testutil.PNG, KindImage, "image/png"},
		{"a.jpg", testutil.JPEG, KindImage, "image/jpeg"},
		{"a.gif", testutil.GIF, KindImage, "image/gif"},
		{"a.webp", testutil.WebP, KindImage, "image/webp"},
		{"a.mp3", testutil.MP3, KindAudio, "audio/mpeg"},
		{"a.wav", testutil.WAV, KindAudio, "audio/wav"},
		{"a.m4a", testutil.M4A, KindAudio, "audio/mp4"},
		{"a.mp4", testutil.MP4, KindVideo, "video/mp4"},
		{"a.mov", testutil.MOV, KindVideo, "video/quicktime"},
		{"a.webm", testutil.WebM, KindVideo, "video/webm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, tc.name, tc.data)
			f, err := Inspect(path)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if f.Kind != tc.kind || f.ContentType != tc.contentType {
				t.Errorf("got kind=%s contentType=%s, want %s %s", f.Kind, f.ContentType, tc.kind, tc.contentType)
			}
			if f.Name != tc.name {
				t.Errorf("Name = %q, want the basename %q", f.Name, tc.name)
			}
			if f.Size != int64(len(tc.data)) {
				t.Errorf("Size = %d, want %d", f.Size, len(tc.data))
			}
		})
	}
}

// The type comes from the content, never from the extension: secrets and
// renderable documents are refused even when named like media.
func TestInspectRejectsNonMediaWhateverTheExtension(t *testing.T) {
	cases := map[string][]byte{
		"id_rsa":      testutil.PrivateKey,
		"id_rsa.png":  testutil.PrivateKey,
		".env":        testutil.DotEnv,
		"env.jpg":     testutil.DotEnv,
		"page.html":   testutil.HTML,
		"page.png":    testutil.HTML,
		"vector.svg":  testutil.SVG,
		"vector.webp": testutil.SVG,
		"tool.mp4":    testutil.ELF,
		"doc.pdf":     testutil.PDF,
		"notes.gif":   []byte("just some plain text, nothing to see here"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Inspect(writeFile(t, name, data))
			if !errors.Is(err, ErrUnsupportedMedia) {
				t.Fatalf("Inspect(%s) error = %v, want ErrUnsupportedMedia", name, err)
			}
		})
	}
}

func TestInspectRejectsDirectory(t *testing.T) {
	_, err := Inspect(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("error = %v, want a directory rejection", err)
	}
}

func TestInspectRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	// The target is a perfectly valid image: the link itself is the problem,
	// because it lets a path in a harmless-looking directory point anywhere.
	target := writeFile(t, "real.png", testutil.PNG)
	link := filepath.Join(t.TempDir(), "link.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := Inspect(link)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("error = %v, want a symbolic link rejection", err)
	}
}

func TestInspectRejectsDeviceFile(t *testing.T) {
	if _, err := os.Lstat("/dev/null"); err != nil {
		t.Skip("no /dev/null")
	}
	if _, err := Inspect("/dev/null"); err == nil {
		t.Fatal("Inspect(/dev/null) succeeded, want a rejection")
	}
}

func TestInspectRejectsMissingAndEmpty(t *testing.T) {
	if _, err := Inspect(filepath.Join(t.TempDir(), "nope.png")); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing file error = %v", err)
	}
	if _, err := Inspect(writeFile(t, "empty.png", nil)); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty file error = %v", err)
	}
	if _, err := Inspect(""); err == nil {
		t.Error("empty path accepted")
	}
}

func TestInspectEnforcesPerTypeSizeLimit(t *testing.T) {
	// A sparse file: the size is checked from metadata, so nothing this
	// large is ever read.
	path := writeFile(t, "big.png", testutil.PNG)
	if err := os.Truncate(path, 20*mb+1); err != nil {
		t.Fatal(err)
	}
	_, err := Inspect(path)
	if err == nil || !strings.Contains(err.Error(), "at most 20 MB") {
		t.Fatalf("error = %v, want the 20 MB image limit", err)
	}

	// The same size is fine for a video, whose limit is 100 MB.
	video := writeFile(t, "ok.mp4", testutil.MP4)
	if err := os.Truncate(video, 20*mb+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(video); err != nil {
		t.Fatalf("20 MB video rejected: %v", err)
	}
}

func TestOpenRefusesFileSwappedAfterInspection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(path, testutil.PNG, 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, testutil.PrivateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	// Swap the approved file for a link to something else.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}
	if rc, err := f.Open(); err == nil {
		rc.Close()
		t.Fatal("Open succeeded on a file that was swapped after inspection")
	}
}

func TestOpenStreamsExactlyTheInspectedBytes(t *testing.T) {
	path := writeFile(t, "a.png", testutil.PNG)
	f, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	// Bytes appended after the check must not be sent.
	extra, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.Write([]byte("APPENDED-AFTER-CHECK")); err != nil {
		t.Fatal(err)
	}
	extra.Close()

	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(testutil.PNG) {
		t.Errorf("stream = %d bytes, want exactly the %d inspected bytes", len(got), len(testutil.PNG))
	}
}

func TestParseRef(t *testing.T) {
	png := writeFile(t, "a.png", testutil.PNG)
	mp4 := writeFile(t, "a.mp4", testutil.MP4)

	for _, remote := range []string{"https://cdn.example.com/a.png", "http://example.com/a.png", "assetId://asset_123"} {
		ref, err := ParseRef(remote, KindImage)
		if err != nil || ref.Remote != remote || ref.File != nil {
			t.Errorf("ParseRef(%q) = %+v, %v; want it passed through", remote, ref, err)
		}
	}

	ref, err := ParseRef(png, KindImage)
	if err != nil || ref.File == nil || ref.File.Kind != KindImage {
		t.Fatalf("ParseRef(local png) = %+v, %v", ref, err)
	}
	if _, err := ParseRef(mp4, KindImage); err == nil || !strings.Contains(err.Error(), "expected an image file") {
		t.Errorf("video passed as --image: error = %v", err)
	}
	if _, err := ParseRef(mp4, ""); err != nil {
		t.Errorf("any-kind ref rejected a video: %v", err)
	}
	if _, err := ParseRef("data:image/png;base64,AAAA", KindImage); err == nil {
		t.Error("data: URI accepted")
	}
	if _, err := ParseRef("  ", KindImage); err == nil {
		t.Error("blank reference accepted")
	}
}
