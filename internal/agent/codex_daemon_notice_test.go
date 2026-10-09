package agent

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// Codex 0.162's TUI runs on a background app-server it starts once and
// leaves running, which built its model list when it started: after magpie
// changes Codex's list, a new codex session still shows Codex's own models
// until that app-server is restarted, while the desktop app, restarted,
// shows magpie's (TJHHHH, luci on Discord). Restarting the app and the open
// sessions, as the advice said, leaves it; the advice after a change names
// it and the command that restarts it, here and in WSL, where the Agents
// row can't tell a stale copy.
func TestCodexAdviceNamesTheSharedAppServer(t *testing.T) {
	home, _ := codexHome(t, "", "")
	was := codexRunning
	codexRunning = func() bool { return true }
	t.Cleanup(func() { codexRunning = was })
	if n := codex(home).Notice(); !strings.Contains(n, provider.CodexDaemonRestart) {
		t.Errorf("Codex's advice leaves out its app-server: %q", n)
	}
	for _, k := range wslKinds {
		if k.id == "codex" && !strings.Contains(k.restart, provider.CodexDaemonRestart) {
			t.Errorf("Codex in WSL's advice leaves out its app-server: %q", k.restart)
		}
	}
	// nothing running, nothing to restart
	codexRunning = func() bool { return false }
	if n := codex(home).Notice(); strings.Contains(n, provider.CodexDaemonRestart) {
		t.Errorf("advice with no Codex running: %q", n)
	}
}
