// Package alerter fires macOS desktop notifications for session-lens events.
// On non-darwin platforms all calls are silent no-ops.
package alerter

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
)

// Notifier is the interface used to send a desktop notification. The default
// implementation shells out to osascript; tests can inject a stub. openURL is
// optional — pass "" to use the configured dashboard URL.
type Notifier interface {
	Notify(title, message, openURL string)
}

// Default returns the platform-appropriate Notifier. On darwin, it prefers
// terminal-notifier (clickable, opens the dashboard) and falls back to
// osascript when terminal-notifier isn't on PATH. Disable entirely with
// SESSIONLENS_DESKTOP_ALERTS=0. Dashboard URL is configurable via
// SESSIONLENS_DASHBOARD_URL (default http://localhost:7821).
func Default() Notifier {
	if runtime.GOOS != "darwin" {
		return noopNotifier{}
	}
	if os.Getenv("SESSIONLENS_DESKTOP_ALERTS") == "0" {
		return noopNotifier{}
	}
	if path, err := exec.LookPath("terminal-notifier"); err == nil {
		return terminalNotifier{binary: path, defaultURL: dashboardURL()}
	}
	logFallbackOnce()
	return osascriptNotifier{}
}

// fallbackLogged ensures we emit the click-through warning at most once per
// process lifetime — Default() can be called multiple times.
var fallbackLogged bool

func logFallbackOnce() {
	if fallbackLogged {
		return
	}
	fallbackLogged = true
	log.Printf("WARN: terminal-notifier not installed; cost-spike notifications won't be clickable. " +
		"Run `brew install terminal-notifier` to enable opening the dashboard from a notification.")
}

func dashboardURL() string {
	if u := os.Getenv("SESSIONLENS_DASHBOARD_URL"); u != "" {
		return u
	}
	return "http://localhost:7821"
}

// Notify is a package-level convenience that calls Default().Notify. Fire and
// forget: it runs inside a goroutine and does not block the caller. Pass ""
// for openURL to use the configured dashboard URL.
func Notify(title, message, openURL string) {
	go Default().Notify(title, message, openURL)
}

// terminalNotifier shells out to `terminal-notifier`. The -open flag attaches
// a URL to the notification so clicking it opens a real browser tab. Per-call
// openURL takes precedence over defaultURL, enabling deep-links like
// "/#session/<id>" that jump straight to the offending session.
type terminalNotifier struct {
	binary     string
	defaultURL string
}

func (t terminalNotifier) Notify(title, message, openURL string) {
	args := []string{
		"-title", title,
		"-message", message,
		"-sender", "com.apple.Safari", // borrowed bundle so the notification has a real icon
	}
	url := openURL
	if url == "" {
		url = t.defaultURL
	}
	if url != "" {
		args = append(args, "-open", url)
	}
	_ = exec.Command(t.binary, args...).Run()
}

// osascriptNotifier fires a macOS Notification Center notification.
// Clicks land in Script Editor — that's why terminalNotifier is preferred.
// openURL is ignored: osascript notifications aren't natively clickable.
type osascriptNotifier struct{}

func (osascriptNotifier) Notify(title, message, _ string) {
	script := fmt.Sprintf(
		`display notification %q with title %q`,
		message, title,
	)
	cmd := exec.Command("osascript", "-e", script)
	// Ignore errors; notifications are best-effort.
	_ = cmd.Run()
}

// noopNotifier silently discards all notifications.
type noopNotifier struct{}

func (noopNotifier) Notify(_, _, _ string) {}
