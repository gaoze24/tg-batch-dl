// Package sysutil opens URLs and folders with the OS shell.
package sysutil

import (
	"os"
	"os/exec"
	"runtime"
)

// OpenURL opens an http(s) URL in the default browser.
func OpenURL(url string) error {
	switch runtime.GOOS {
	case "windows":
		return start("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return start("open", url)
	default:
		return start("xdg-open", url)
	}
}

// OpenDir shows a folder in the file manager, creating it first if needed.
func OpenDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "windows":
		return start("explorer", path)
	case "darwin":
		return start("open", path)
	default:
		return start("xdg-open", path)
	}
}

// start launches a helper without waiting for it, reaping it in the background.
func start(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
