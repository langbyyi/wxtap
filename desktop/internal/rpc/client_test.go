package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCore echoes a scripted response for every request line it receives.
type fakeCore struct {
	mu        sync.Mutex
	cond      *sync.Cond
	lines     []string
	responser func(line string) string
}

func newFakeCore(responser func(line string) string) (*fakeCore, *io.PipeWriter, *io.PipeReader) {
	core := &fakeCore{responser: responser}
	core.cond = sync.NewCond(&core.mu)
	reader, clientSideOfCoreStdin := io.Pipe()
	coreSideOfStdout, writer := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			core.mu.Lock()
			core.lines = append(core.lines, scanner.Text())
			core.mu.Unlock()
			if reply := responser(scanner.Text()); reply != "" {
				_, _ = writer.Write([]byte(reply + "\n"))
			}
		}
	}()
	return core, clientSideOfCoreStdin, coreSideOfStdout
}

func TestCallCorrelatesResponsesAndDecodesResults(t *testing.T) {
	_, stdin, stdout := newFakeCore(func(line string) string {
		var req struct {
			ID   int64           `json:"id"`
			Path json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		return `{"id":` + itoa(req.ID) + `,"result":{"echo":true}}`
	})
	client := NewClient(stdout, stdin)
	defer func() { _ = client.Close() }()

	var result struct {
		Echo bool `json:"echo"`
	}
	err := client.Call(context.Background(), "engine.status", map[string]any{}, &result)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.Echo {
		t.Fatal("result not decoded")
	}
}

func TestCallSurfacesTypedCoreErrors(t *testing.T) {
	_, stdin, stdout := newFakeCore(func(line string) string {
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		return `{"id":` + itoa(req.ID) + `,"error":{"code":2001,"message":"engine not running","retryable":true}}`
	})
	client := NewClient(stdout, stdin)
	defer func() { _ = client.Close() }()

	err := client.Call(context.Background(), "engine.start", map[string]any{}, nil)
	coreErr, ok := err.(*CoreError)
	if !ok {
		t.Fatalf("expected *CoreError, got %T: %v", err, err)
	}
	if coreErr.Code != 2001 || coreErr.Message != "engine not running" || !coreErr.Retryable {
		t.Fatalf("unexpected error fields: %+v", coreErr)
	}
}

func TestCallRejectsWhenCoreNeverAnswers(t *testing.T) {
	_, stdin, stdout := newFakeCore(func(string) string { return "" })
	client := NewClient(stdout, stdin)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.Call(ctx, "engine.status", map[string]any{}, nil); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestConcurrentCallsAreCorrelated(t *testing.T) {
	_, stdin, stdout := newFakeCore(func(line string) string {
		var req struct {
			ID    int64 `json:"id"`
			Param struct {
				N int64 `json:"n"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		return `{"id":` + itoa(req.ID) + `,"result":{"n":` + itoa(req.Param.N) + `}}`
	})
	client := NewClient(stdout, stdin)
	defer func() { _ = client.Close() }()

	const workers = 20
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(n int64) {
			var result struct {
				N int64 `json:"n"`
			}
			err := client.Call(context.Background(), "engine.status", map[string]any{"n": n}, &result)
			if err == nil && result.N != n {
				err = io.ErrUnexpectedEOF
			}
			errs <- err
		}(int64(i))
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
}

func TestEventsAreDispatchedToSubscribers(t *testing.T) {
	_, stdin, stdout := newFakeCore(func(line string) string {
		// Every request also triggers an event push before the response.
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		_ = req
		return `{"event":"engine.status","payload":{"frida":true}}` + "\n" + `{"id":` + itoa(req.ID) + `,"result":{}}`
	})
	client := NewClient(stdout, stdin)
	defer func() { _ = client.Close() }()

	// The fake core pushes its event as a side effect of handling any request.
	if err := client.Call(context.Background(), "engine.status", map[string]any{}, nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	select {
	case event := <-client.Events():
		if event.Name != "engine.status" {
			t.Fatalf("unexpected event %q", event.Name)
		}
		var payload struct {
			Frida bool `json:"frida"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || !payload.Frida {
			t.Fatalf("payload: %v %+v", err, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not received")
	}
}

func itoa(n int64) string {
	return json.Number(int64String(n)).String()
}

func int64String(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// The reader has to stop at the cap instead of collecting a line the Core
// never terminates: ReadBytes grew the buffer to the size of the line before
// readLoop could reject it.
func TestReadBoundedLineStopsAtLimit(t *testing.T) {
	// A line longer than the bufio buffer must still arrive whole.
	reader := bufio.NewReaderSize(strings.NewReader("0123456789\nnext"), 4)
	line, err := readBoundedLine(reader, 64)
	if err != nil || string(line) != "0123456789\n" {
		t.Fatalf("line through a small buffer: %q %v", line, err)
	}

	// Over the cap the read stops early with the typed error.
	reader = bufio.NewReaderSize(strings.NewReader(strings.Repeat("a", 4096)+"\n"), 32)
	if _, err := readBoundedLine(reader, 64); !errors.Is(err, errLineTooLong) {
		t.Fatalf("oversized line: %v", err)
	}

	// A final unterminated line still reaches the caller with EOF.
	reader = bufio.NewReaderSize(strings.NewReader("tail"), 4)
	line, err = readBoundedLine(reader, 64)
	if !errors.Is(err, io.EOF) || string(line) != "tail" {
		t.Fatalf("unterminated line: %q %v", line, err)
	}
}

// A drain page with base64 bodies can easily exceed the old 4MB scanner
// cap; the client must read it instead of tearing down the connection.
func TestLargeResponseLineIsNotTruncated(t *testing.T) {
	big := strings.Repeat("x", 5*1024*1024)
	_, stdin, stdout := newFakeCore(func(line string) string {
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(line), &req)
		return `{"id":` + itoa(req.ID) + `,"result":{"body":"` + big + `"}}`
	})
	client := NewClient(stdout, stdin)
	defer func() { _ = client.Close() }()

	var result struct {
		Body string `json:"body"`
	}
	if err := client.Call(context.Background(), "hook.drain", map[string]any{}, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(result.Body) != len(big) {
		t.Fatalf("body truncated: %d != %d", len(result.Body), len(big))
	}
}

// A Core whose event loop is wedged blocks stdin writes while holding
// writeMu; a caller with a deadline must get a typed timeout instead of
// hanging forever behind the stuck write.
func TestCallTimesOutWhenStdinBlocks(t *testing.T) {
	blockingStdin := newBlockedWriter()
	client := NewClient(strings.NewReader(""), blockingStdin)
	defer func() { _ = client.Close() }()

	start := time.Now()
	err := client.Call(withTimeout(t, 50*time.Millisecond), "engine.start", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("call hung %v instead of honoring the deadline", elapsed)
	}
}

// A cancelled call tears the connection down and marks it dead (Done): the
// engine layer's respawn path keys on that signal, because the Core process
// itself can linger with a dead stdin. Every later call must fail fast with
// the same typed error, and a close must stay idempotent.
func TestCancelledWriteMarksConnectionDead(t *testing.T) {
	blockingStdin := newBlockedWriter()
	client := NewClient(strings.NewReader(""), blockingStdin)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Call(ctx, "engine.start", map[string]any{}, nil)
	}()
	// Let the call park inside the blocked write, then cancel it.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected a cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled call never returned")
	}

	select {
	case <-client.Done():
	default:
		t.Fatal("a torn-down connection must report itself through Done")
	}
	err := client.Call(context.Background(), "engine.status", map[string]any{}, nil)
	coreErr, ok := err.(*CoreError)
	if !ok || coreErr.Code != 1001 {
		t.Fatalf("post-teardown call = %v, want CoreError 1001", err)
	}
	// A close after the teardown must be a no-op, not a double close.
	_ = client.Close()
	select {
	case <-client.Done():
	default:
		t.Fatal("Done must stay closed after an idempotent Close")
	}
}

func newBlockedWriter() io.WriteCloser {
	return &blockedWriter{ready: make(chan struct{})}
}

type blockedWriter struct {
	ready chan struct{}
	once  sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	<-w.ready
	return 0, io.ErrClosedPipe
}

func (w *blockedWriter) Close() error {
	w.once.Do(func() { close(w.ready) })
	return nil
}

func withTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
