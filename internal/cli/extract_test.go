package cli

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
)

// extractKeys must not reach sudo, WeChat or the network when it has no
// terminal, because sudo needs one for the password.
func TestExtractRefusesWithoutTerminal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("key extraction is macOS only")
	}
	app := &App{Out: io.Discard, Err: io.Discard}
	err := app.extractKeys(context.Background(), "", 180, false)
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("extractKeys = %v, want a terminal error", err)
	}
}
