package ipc

import "testing"

func TestTaskTrackerLifecyclePublishesSnapshots(t *testing.T) {
	var got []TaskState
	tracker := NewTaskTracker(func(state TaskState) { got = append(got, state) })
	started := tracker.Start("extract", "开始反编译", 4)
	if started.ID == "" || started.Phase != "running" {
		t.Fatalf("unexpected start: %+v", started)
	}
	if _, ok := tracker.Update(started.ID, "scanning", "扫描资源", 2, 4); !ok {
		t.Fatal("update rejected")
	}
	finished, ok := tracker.Finish(started.ID, "done", "完成", "")
	if !ok || finished.FinishedAt.IsZero() || finished.Phase != "done" {
		t.Fatalf("unexpected finish: %+v", finished)
	}
	if len(got) != 3 || got[1].Current != 2 {
		t.Fatalf("snapshots = %+v", got)
	}
}

func TestTaskTrackerRejectsUnknownTask(t *testing.T) {
	tracker := NewTaskTracker(nil)
	if _, ok := tracker.Update("missing", "running", "", 0, 0); ok {
		t.Fatal("unknown update accepted")
	}
	if _, ok := tracker.Finish("missing", "failed", "", "x"); ok {
		t.Fatal("unknown finish accepted")
	}
}

func TestTaskTrackerCancel(t *testing.T) {
	tracker := NewTaskTracker(nil)
	started := tracker.Start("scan", "扫描", 10)
	state, ok := tracker.Cancel(started.ID, "用户取消")
	if !ok || state.Phase != "cancelled" || state.Message != "用户取消" {
		t.Fatalf("unexpected cancellation: %+v", state)
	}
}
