// Package rpc speaks the Core's newline-delimited JSON-RPC protocol over an
// arbitrary byte stream (typically a child process's stdio).
//
// Wire format (matches core/src/rpc/protocol.ts):
//
//	request  {"id":1,"method":"engine.start","params":{...}}
//	response {"id":1,"result":...} | {"id":1,"error":{"code":N,"message":"...","retryable":bool}}
//	event    {"event":"engine.status","payload":...}   (no id)
package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
)

// CoreError is a structured failure reported by the Core.
type CoreError struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *CoreError) Error() string {
	return fmt.Sprintf("core error %d: %s", e.Code, e.Message)
}

// Event is an unsolicited JSON line pushed by the Core.
type Event struct {
	Name    string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

type pendingCall struct {
	ch chan rpcResponse
}

type rpcResponse struct {
	result json.RawMessage
	err    *CoreError
}

// Client multiplexes concurrent calls over one Core connection and exposes
// pushed events through an exported channel.
type Client struct {
	stdin io.WriteCloser

	writeMu sync.Mutex
	pendMu  sync.Mutex
	pending map[int64]pendingCall
	nextID  int64

	events    chan Event
	closed    chan struct{}
	closeOnce sync.Once
	// eventsOnce guards close(c.events): teardown and the read loop's EOF path
	// can both reach failAll, and a second close would panic the reader.
	eventsOnce sync.Once
}

// NewClient starts reading responses and events from r and writing requests to w.
func NewClient(r io.Reader, stdin io.WriteCloser) *Client {
	c := &Client{
		stdin:   stdin,
		pending: make(map[int64]pendingCall),
		events:  make(chan Event, 16),
		closed:  make(chan struct{}),
	}
	go c.readLoop(r)
	return c
}

// Events returns the channel receiving Core-pushed events.
func (c *Client) Events() <-chan Event {
	return c.events
}

// Call sends a request and blocks until the correlated response or ctx expiry.
// When result is non-nil the response payload is decoded into it.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	select {
	case <-c.closed:
		return &CoreError{Code: 1001, Message: "core connection closed", Retryable: true}
	default:
	}

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}

	c.pendMu.Lock()
	c.nextID++
	id := c.nextID
	call := pendingCall{ch: make(chan rpcResponse, 1)}
	c.pending[id] = call
	c.pendMu.Unlock()

	request, err := json.Marshal(map[string]any{
		"id":     id,
		"method": method,
		"params": json.RawMessage(paramsJSON),
	})
	if err != nil {
		c.removePending(id)
		return fmt.Errorf("marshal request: %w", err)
	}

	if err := c.writeLine(ctx, request); err != nil {
		c.removePending(id)
		return &CoreError{Code: 1001, Message: "core connection closed: " + err.Error(), Retryable: true}
	}

	select {
	case resp := <-call.ch:
		if resp.err != nil {
			return resp.err
		}
		if result != nil {
			if err := json.Unmarshal(resp.result, result); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		c.removePending(id)
		return &CoreError{Code: 1002, Message: "core call timed out: " + method, Retryable: true}
	}
}

// OnEvent registers a callback invoked for every unsolicited Core event.
//
// The channel is deliberately small (16) and drops when no consumer keeps up:
// this is a debug stream, and the Core keeps a bounded buffer of its own. The
// callback runs on one goroutine; register it once (the channel is closed when
// the connection fails, which ends the loop).
func (c *Client) OnEvent(callback func(event Event)) {
	go func() {
		for event := range c.events {
			callback(event)
		}
	}()
}

// Close terminates the connection; all pending calls fail with a retryable error.
func (c *Client) Close() error {
	return c.teardown("client closed")
}

// Done closes when the connection is torn down — by Close, or by writeLine's
// wedged-writer teardown. The engine layer watches it alongside process exit:
// a torn stdin with a live process is a zombie only this signal reveals, and
// the respawn path must replace it instead of answering every call with 1001.
func (c *Client) Done() <-chan struct{} {
	return c.closed
}

// teardown marks the connection dead, fails every pending call with a
// retryable error and closes stdin. writeLine's cancellation branch depends on
// this doing all three: closing stdin alone leaves the Core pinned alive by
// its listening servers while every later write answers "file already closed"
// — a zombie nothing respawned, because the process never exited.
func (c *Client) teardown(reason string) error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		c.failAll(&CoreError{Code: 1001, Message: "core connection closed: " + reason, Retryable: true})
		err = c.stdin.Close()
	})
	return err
}

func (c *Client) removePending(id int64) {
	c.pendMu.Lock()
	delete(c.pending, id)
	c.pendMu.Unlock()
}

// writeLine sends one request line, honoring ctx. A wedged Core blocks the
// stdin write while this mutex is held, so a blocked write tears the
// connection down: every pending call fails and the next call respawns the
// process instead of everyone hanging forever behind the stuck writer.
func (c *Client) writeLine(ctx context.Context, line []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	errc := make(chan error, 1)
	go func() {
		_, err := c.stdin.Write(append(line, '\n'))
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		// 写可能恰好与取消同时完成——先非阻塞确认，别把一条健康连接错杀。
		select {
		case err := <-errc:
			return err
		default:
		}
		go func() { <-errc }()
		// The cancellation itself is torn-down-and-marked-dead (teardown), not
		// a bare stdin.Close: a caller walking away must cost its own request,
		// not leave a stdin-closed process that every later call answers with
		// "file already closed" and nothing ever respawns.
		_ = c.teardown(ctx.Err().Error())
		return ctx.Err()
	}
}

// maxLineSize bounds one response line; a drain page with base64 bodies
// routinely exceeds the old 4MB Scanner cap, so the reader only refuses
// absurdly large lines instead of tearing down the connection.
const maxLineSize = 64 * 1024 * 1024

func (c *Client) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := readBoundedLine(reader, maxLineSize)
		if errors.Is(err, errLineTooLong) {
			c.failAll(&CoreError{Code: 1001, Message: "core response line too large", Retryable: true})
			return
		}
		if idx := bytes.LastIndexByte(line, '\n'); idx >= 0 {
			line = line[:idx]
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			// EOF or read error: fail every pending call with a retryable error.
			reason := "eof"
			if err != io.EOF {
				reason = err.Error()
			}
			c.failAll(&CoreError{Code: 1001, Message: "core connection lost: " + reason, Retryable: true})
			return
		}
	}
}

// errLineTooLong reports a Core line beyond maxLineSize.
var errLineTooLong = errors.New("core response line too large")

// readBoundedLine reads one newline-terminated line without ever buffering
// more than limit bytes. ReadBytes would collect the whole line before the
// caller could check its size, so a runaway Core could still grow this
// process to the size of the line it never terminates.
func readBoundedLine(r *bufio.Reader, limit int) ([]byte, error) {
	line := make([]byte, 0, 64*1024)
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}
func (c *Client) handleLine(line []byte) {
	var probe struct {
		ID    *int64          `json:"id"`
		Event *string         `json:"event"`
		Error *CoreError      `json:"error"`
		Raw   json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		log.Printf("[rpc] dropping malformed core line: %v", err)
		return
	}

	if probe.Event != nil {
		select {
		case c.events <- Event{Name: *probe.Event, Payload: extractPayload(line)}:
		default:
			// No consumer and buffer full: drop the event rather than block reads.
		}
		return
	}

	if probe.ID == nil {
		return
	}
	c.pendMu.Lock()
	call, ok := c.pending[*probe.ID]
	if ok {
		delete(c.pending, *probe.ID)
	}
	c.pendMu.Unlock()
	if !ok {
		return
	}

	if probe.Error != nil {
		call.ch <- rpcResponse{err: probe.Error}
		return
	}
	var body struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(line, &body)
	call.ch <- rpcResponse{result: body.Result}
}

func (c *Client) failAll(err *CoreError) {
	c.pendMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]pendingCall)
	c.pendMu.Unlock()
	for _, call := range pending {
		call.ch <- rpcResponse{err: err}
	}
	c.eventsOnce.Do(func() { close(c.events) })
}

func extractPayload(line []byte) json.RawMessage {
	var body struct {
		Payload json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(line, &body)
	return body.Payload
}
