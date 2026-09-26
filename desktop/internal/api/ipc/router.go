// Package ipc implements the desktop backend method surface the Vue
// frontend speaks: call(method, paramsJSON) → IPC envelope `{result}|{error}`.
// Each method is registered on the Router by the desktop shell and forwards
// to the Go domain packages.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
)

// Handler processes one IPC method call.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Router dispatches IPC method names to handlers.
type Router struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// New creates an empty router.
func New() *Router {
	return &Router{handlers: map[string]Handler{}}
}

// Register binds a method name (must not be empty).
func (r *Router) Register(method string, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[method] = handler
}

// Call dispatches one IPC call; unknown methods fail clearly. A handler panic
// is reported as an ordinary error: the shell (and the MCP server driving the
// same router) must survive a bad handler instead of dying with it.
func (r *Router) Call(ctx context.Context, method string, params json.RawMessage) (result any, err error) {
	r.mu.RLock()
	handler, ok := r.handlers[method]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unsupported backend method: %s", method)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("[ipc] %s panicked: %v\n%s", method, recovered, debug.Stack())
			result = nil
			err = fmt.Errorf("backend method %s panicked: %v", method, recovered)
		}
	}()
	return handler(ctx, params)
}

// Methods lists the registered method names (sorted for stable display).
func (r *Router) Methods() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}
