package ipc

import (
	"fmt"
	"sync"
	"time"
)

// TaskState is the stable lifecycle payload shared by long-running GUI jobs.
type TaskState struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Phase      string    `json:"phase"`
	Current    int       `json:"current,omitempty"`
	Total      int       `json:"total,omitempty"`
	Message    string    `json:"message,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// TaskTracker keeps task state in one place and emits immutable snapshots.
// A callback may forward snapshots to Wails events or tests.
type TaskTracker struct {
	mu     sync.RWMutex
	tasks  map[string]TaskState
	emit   func(TaskState)
	serial uint64
}

func NewTaskTracker(emit func(TaskState)) *TaskTracker {
	return &TaskTracker{tasks: make(map[string]TaskState), emit: emit}
}

func (t *TaskTracker) Start(kind, message string, total int) TaskState {
	t.mu.Lock()
	t.serial++
	id := fmt.Sprintf("%s-%d", kind, t.serial)
	state := TaskState{ID: id, Kind: kind, Phase: "running", Total: total, Message: message, StartedAt: time.Now().UTC()}
	t.tasks[id] = state
	t.mu.Unlock()
	t.publish(state)
	return state
}

func (t *TaskTracker) Update(id, phase, message string, current, total int) (TaskState, bool) {
	t.mu.Lock()
	state, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return TaskState{}, false
	}
	state.Phase, state.Message, state.Current = phase, message, current
	if total > 0 {
		state.Total = total
	}
	t.tasks[id] = state
	t.mu.Unlock()
	t.publish(state)
	return state, true
}

func (t *TaskTracker) Finish(id, phase, message, taskError string) (TaskState, bool) {
	t.mu.Lock()
	state, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return TaskState{}, false
	}
	state.Phase, state.Message, state.Error, state.FinishedAt = phase, message, taskError, time.Now().UTC()
	t.tasks[id] = state
	t.mu.Unlock()
	t.publish(state)
	return state, true
}

// Cancel marks an active task as cancelled while preserving its last progress.
func (t *TaskTracker) Cancel(id, message string) (TaskState, bool) {
	return t.Finish(id, "cancelled", message, "")
}

func (t *TaskTracker) Get(id string) (TaskState, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state, ok := t.tasks[id]
	return state, ok
}

func (t *TaskTracker) publish(state TaskState) {
	if t.emit != nil {
		t.emit(state)
	}
}
