package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/geniuspath/apipod-cli/internal/testutil"
)

// storage is a fake object-storage host that records what it is sent.
type storage struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
}

func newStorage(t *testing.T, handler http.HandlerFunc) *storage {
	t.Helper()
	s := &storage{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func servePNG(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(testutil.PNG)
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func TestSaveWritesResultUnderCLIGeneratedName(t *testing.T) {
	s := newStorage(t, servePNG)
	dir := t.TempDir()
	d := NewDownloaderWithClient(s.Client())

	path, err := d.Save(context.Background(), s.URL+"/whatever/name.exe", dir, "img_abc123", 1)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if want := filepath.Join(dir, "img_abc123-1.png"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(testutil.PNG) {
		t.Errorf("saved content mismatch (err=%v)", err)
	}
	if entries := dirEntries(t, dir); len(entries) != 1 {
		t.Errorf("directory holds %v, want only the result (no temp file left behind)", entries)
	}
}

func TestSaveNeverSendsAuthorization(t *testing.T) {
	s := newStorage(t, servePNG)
	// Even a client whose transport was (wrongly) handed a credential-bearing
	// environment must not attach one: the downloader builds a bare request.
	t.Setenv("APIPOD_API_KEY", "sk-should-never-leave")
	d := NewDownloaderWithClient(s.Client())
	if _, err := d.Save(context.Background(), s.URL+"/r.png", t.TempDir(), "job", 1); err != nil {
		t.Fatal(err)
	}
	if len(s.headers) != 1 {
		t.Fatalf("storage saw %d requests, want 1", len(s.headers))
	}
	for name, values := range s.headers[0] {
		if name == "Authorization" || name == "Cookie" || name == "X-Api-Key" {
			t.Errorf("download request carried %s: %v", name, values)
		}
		for _, v := range values {
			if strings.Contains(v, "sk-should-never-leave") {
				t.Errorf("credential leaked in header %s", name)
			}
		}
	}
}

func TestSaveRefusesNonHTTPS(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(servePNG))
	defer plain.Close()
	dir := t.TempDir()
	d := NewDownloader()
	for _, u := range []string{plain.URL + "/r.png", "file:///etc/passwd", "ftp://example.com/r.png", "/etc/passwd", "not a url"} {
		if _, err := d.Save(context.Background(), u, dir, "job", 1); err == nil {
			t.Errorf("Save(%q) succeeded, want a refusal", u)
		}
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("refused downloads left files behind: %v", entries)
	}
}

func TestSaveRefusesRedirectToHTTP(t *testing.T) {
	hits := 0
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		servePNG(w, r)
	}))
	defer plain.Close()
	s := newStorage(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/r.png", http.StatusFound)
	})
	d := NewDownloaderWithClient(s.Client())
	if _, err := d.Save(context.Background(), s.URL+"/r.png", t.TempDir(), "job", 1); err == nil {
		t.Fatal("followed a redirect from https to http")
	}
	if hits != 0 {
		t.Errorf("the http target was requested %d times, want 0", hits)
	}
}

// A hostile server cannot steer the file outside the output directory: not
// through the URL path, not through Content-Disposition, not through the id.
func TestSaveResistsPathTraversal(t *testing.T) {
	s := newStorage(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../../../tmp/evil.sh"`)
		servePNG(w, r)
	})
	parent := t.TempDir()
	dir := filepath.Join(parent, "out")
	d := NewDownloaderWithClient(s.Client())

	for i, jobID := range []string{"../../escape", "..", "/etc/cron.d/x", `..\..\win`, "a/b/c", ".hidden", "-rf"} {
		path, err := d.Save(context.Background(), s.URL+"/..%2f..%2f..%2fetc%2fpasswd", dir, jobID, i+1)
		if err != nil {
			t.Fatalf("Save(jobID=%q): %v", jobID, err)
		}
		if filepath.Dir(path) != dir {
			t.Errorf("jobID %q: file written to %q, outside %q", jobID, path, dir)
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "-") || strings.ContainsAny(base, `/\`) {
			t.Errorf("jobID %q produced unsafe file name %q", jobID, base)
		}
		if !strings.HasSuffix(base, ".png") {
			t.Errorf("jobID %q: name %q does not end in the sniffed extension", jobID, base)
		}
	}
	if entries := dirEntries(t, parent); len(entries) != 1 || entries[0] != "out" {
		t.Errorf("parent directory holds %v, want only the output directory", entries)
	}
}

func TestSaveDoesNotOverwriteByDefault(t *testing.T) {
	s := newStorage(t, servePNG)
	dir := t.TempDir()
	existing := filepath.Join(dir, "job-1.png")
	if err := os.WriteFile(existing, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewDownloaderWithClient(s.Client())

	_, err := d.Save(context.Background(), s.URL+"/r.png", dir, "job", 1)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want an already-exists refusal", err)
	}
	if got, _ := os.ReadFile(existing); string(got) != "precious" {
		t.Errorf("existing file was modified: %q", got)
	}
	if entries := dirEntries(t, dir); len(entries) != 1 {
		t.Errorf("refused download left files behind: %v", entries)
	}

	d.Force = true
	if _, err := d.Save(context.Background(), s.URL+"/r.png", dir, "job", 1); err != nil {
		t.Fatalf("Save with Force: %v", err)
	}
	if got, _ := os.ReadFile(existing); string(got) != string(testutil.PNG) {
		t.Error("Force did not replace the existing file")
	}
}

// A download that fails part-way leaves neither a partial result nor a temp
// file: the final name only ever appears complete.
func TestSaveIsAtomicOnFailure(t *testing.T) {
	s := newStorage(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write(testutil.PNG) // then the connection drops short
	})
	dir := t.TempDir()
	d := NewDownloaderWithClient(s.Client())
	if _, err := d.Save(context.Background(), s.URL+"/r.png", dir, "job", 1); err == nil {
		t.Fatal("truncated download reported success")
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("failed download left files behind: %v", entries)
	}
}

func TestSaveEnforcesSizeLimit(t *testing.T) {
	big := append(append([]byte{}, testutil.PNG...), make([]byte, 5000)...)
	for _, declare := range []bool{true, false} {
		s := newStorage(t, func(w http.ResponseWriter, _ *http.Request) {
			if !declare {
				// Chunked: the limit must hold without a Content-Length.
				w.Header().Set("Transfer-Encoding", "chunked")
				w.(http.Flusher).Flush()
			}
			_, _ = w.Write(big)
		})
		dir := t.TempDir()
		d := NewDownloaderWithClient(s.Client())
		d.MaxBytes = 4000
		_, err := d.Save(context.Background(), s.URL+"/r.png", dir, "job", 1)
		if err == nil || !strings.Contains(err.Error(), "download limit") {
			t.Errorf("declared=%v: error = %v, want the size limit", declare, err)
		}
		if entries := dirEntries(t, dir); len(entries) != 0 {
			t.Errorf("declared=%v: oversized download left files behind: %v", declare, entries)
		}
	}
}

func TestSaveUsesBinForUnknownContent(t *testing.T) {
	s := newStorage(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png") // a lie
		_, _ = w.Write(testutil.HTML)
	})
	path, err := NewDownloaderWithClient(s.Client()).Save(context.Background(), s.URL+"/r.html", t.TempDir(), "job", 1)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "job-1.bin" {
		t.Errorf("name = %q, want job-1.bin: the extension must follow the content, not the server", filepath.Base(path))
	}
}

func TestSaveReportsHTTPErrors(t *testing.T) {
	s := newStorage(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusNotFound) })
	dir := t.TempDir()
	if _, err := NewDownloaderWithClient(s.Client()).Save(context.Background(), s.URL+"/r.png", dir, "job", 1); err == nil {
		t.Fatal("404 reported as success")
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("failed download left files behind: %v", entries)
	}
}
