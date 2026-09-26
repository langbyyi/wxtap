package ipc

import (
	"context"
	"sync"
	"testing"
)

func TestConfigStoreConcurrentSaveKeepsBothPatches(t *testing.T) {
	store := NewConfigStore(t.TempDir())
	var wg sync.WaitGroup
	for key, value := range map[string]string{"alpha": "one", "beta": "two"} {
		wg.Add(1)
		go func(key, value string) {
			defer wg.Done()
			if err := store.Save(context.Background(), map[string]any{key: value}); err != nil {
				t.Errorf("save %s: %v", key, err)
			}
		}(key, value)
	}
	wg.Wait()

	config, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config["alpha"] != "one" || config["beta"] != "two" {
		t.Fatalf("concurrent saves lost data: %#v", config)
	}
}
