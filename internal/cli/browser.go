package cli

import (
	"fmt"
	"os/exec"
	goruntime "runtime"
)

// openBrowser launches the platform's URL handler. The URL is passed as a
// single argv element — no shell is involved, so it cannot be reinterpreted.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
