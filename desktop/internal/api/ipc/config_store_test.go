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

// 调试过的小程序名是 appid 唯一的持久来源（反编译产物和流量库都只有
// appid）：注册表要能合并多次上报、忽略空值、名字变化时覆盖旧值。
func TestConfigStoreRememberAppName(t *testing.T) {
	store := NewConfigStore(t.TempDir())
	if err := store.RememberAppName(" wx-app-a ", "小程序A"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := store.RememberAppName("wx-app-b", "小程序B"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	// 同名重复上报不落盘（不报错即可）；改名则覆盖。
	if err := store.RememberAppName("wx-app-a", "小程序A"); err != nil {
		t.Fatalf("remember unchanged: %v", err)
	}
	if err := store.RememberAppName("wx-app-b", "小程序B改"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	names := store.AppNames()
	if names["wx-app-a"] != "小程序A" || names["wx-app-b"] != "小程序B改" || len(names) != 2 {
		t.Fatalf("appNames = %#v", names)
	}

	// 空值与空注册表都是安全的；nil store 也直接透传。
	if err := store.RememberAppName("", "ignored"); err != nil {
		t.Fatalf("empty appid: %v", err)
	}
	if err := store.RememberAppName("wx-app-a", "  "); err != nil {
		t.Fatalf("empty name: %v", err)
	}
	var nilStore *ConfigStore
	if got := nilStore.AppNames(); got != nil {
		t.Fatalf("nil store AppNames = %#v, want nil", got)
	}
	if err := nilStore.RememberAppName("wx-x", "y"); err != nil {
		t.Fatalf("nil store remember: %v", err)
	}
}
