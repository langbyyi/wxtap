package cloud

import (
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/engine"
)

func TestConvertPreservesAppIDForCorrelation(t *testing.T) {
	record := Convert("wxapi", engine.HookDrainedRecord{
		Seq: 7,
		Record: map[string]any{
			"type": "wx.request", "name": "login", "appId": "wx-app-1", "ts": float64(1),
			"data": map[string]any{"url": "https://api.example.com/login"},
		},
	})
	if record.AppID != "wx-app-1" {
		t.Fatalf("AppID = %q, want wx-app-1", record.AppID)
	}

	rec := Convert("wxapi", engine.HookDrainedRecord{
		Seq: 8,
		Record: map[string]any{
			"type": "wx.request", "name": "old", "appid": "wx-old-1", "ts": float64(2),
		},
	})
	if rec.AppID != "wx-old-1" {
		t.Fatalf("lowercase appid = %q, want wx-old-1", rec.AppID)
	}
}
func TestConvertIDsSeparateAppsAtSameSequence(t *testing.T) {
	recordFor := func(appID string) engine.HookDrainedRecord {
		return engine.HookDrainedRecord{
			Seq: 7,
			Record: map[string]any{
				"type": "wx.request", "name": "login", "appId": appID, "ts": float64(1700000000000),
			},
		}
	}
	first := Convert("wxapi", recordFor("wx-app-1"))
	second := Convert("wxapi", recordFor("wx-app-2"))
	if first.ID == second.ID {
		t.Fatalf("different appids produced the same primary key %q", first.ID)
	}
}
