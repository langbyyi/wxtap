package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/rpc"
)

// TestRealCoreBundleSpeaksTheGoContract drives the shipped Core bundle through
// the production engine client. The fake-Core integration test proves the
// desktop talks to our mock; this is the only offline check that the real
// bundle's field names and result shapes match the Go structs, so a contract
// drift shows up here instead of on a device.
//
// It skips when Node.js or core/dist/cli.js is absent (a fresh checkout without
// `npm run build`, or the CI legs that do not stage the bundle).
func TestRealCoreBundleSpeaksTheGoContract(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	script := resolveCoreScript(exeDir())
	if _, err := os.Stat(script); err != nil {
		t.Skipf("core bundle not built (%s): %v", script, err)
	}

	client, err := engine.StartProcess(exec.Command(nodePath, script))
	if err != nil {
		t.Fatalf("start core: %v", err)
	}
	defer func() { _ = client.Stop(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("engine.status against the real bundle: %v", err)
	}
	if status.Frida || status.Miniapp || status.Devtools {
		t.Fatalf("offline bundle must report no attachments: %+v", status)
	}

	formatted, err := client.CodeFormat(ctx, `{"a":1,"b":[1,2]}`, "json")
	if err != nil {
		t.Fatalf("code.format against the real bundle: %v", err)
	}
	if !strings.Contains(formatted, "\n") || !strings.Contains(formatted, `"a": 1`) {
		t.Fatalf("code.format did not pretty-print JSON: %q", formatted)
	}

	// The error contract matters as much as the success shape: the UI renders
	// these messages and relies on the retryable flag.
	var unknownHook *rpc.CoreError
	if _, err := client.HookDrain(ctx, "nope", 0, 10, 0, 200); !errors.As(err, &unknownHook) {
		t.Fatalf("unknown hook must surface a typed CoreError, got %v", err)
	} else if !strings.Contains(unknownHook.Message, "hook name must be one of") {
		t.Fatalf("unexpected core error message: %v", unknownHook)
	}

	var noDrain *rpc.CoreError
	if _, err := client.HookDrain(ctx, "navigator", 0, 10, 0, 200); !errors.As(err, &noDrain) {
		t.Fatalf("hook without drain must surface a typed CoreError, got %v", err)
	} else if !strings.Contains(noDrain.Message, "does not support drain") {
		t.Fatalf("unexpected core error message: %v", noDrain)
	}

	var notStarted *rpc.CoreError
	if _, err := client.Evaluate(ctx, "1+1", 1000); !errors.As(err, &notStarted) {
		t.Fatalf("evaluate without a running engine must be a typed CoreError, got %v", err)
	} else if notStarted.Message != "engine not started" || !notStarted.Retryable {
		t.Fatalf("offline evaluate error shape changed: %+v", notStarted)
	}
}
