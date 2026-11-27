//go:build unix

package server

import (
	"os"
	"testing"

	"github.com/Autumn-27/artex/internal/toolguard"
)

func TestToolguardBackgroundTrampolineDetection(t *testing.T) {
	t.Setenv(toolguard.BackgroundExeEnv, "/app/artex")
	argv := []string{"bash", "-c", `exec "$_ARTEX_BG_EXE" ` + toolguard.BackgroundArg}
	if !toolguard.IsBackgroundTrampoline(argv) {
		t.Fatalf("expected background trampoline to be recognized for %v", argv)
	}
	os.Unsetenv(toolguard.BackgroundExeEnv)
	if toolguard.IsBackgroundTrampoline(argv) {
		t.Fatalf("expected nested bash without %s not to match background trampoline", toolguard.BackgroundExeEnv)
	}
}
