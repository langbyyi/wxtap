package assets

import (
	"reflect"
	"testing"
)

func TestFilter(t *testing.T) {
	inv := buildFixtureInventory(t)

	byKind := inv.Filter("api", "", "")
	if byKind.Total != 4 || len(byKind.Items) != 4 {
		t.Fatalf("kind filter total = %d, want 4", byKind.Total)
	}
	if !reflect.DeepEqual(byKind.ByKind, map[string]int{KindAPI: 4}) {
		t.Fatalf("kind filter byKind = %+v", byKind.ByKind)
	}
	if !reflect.DeepEqual(byKind.Hosts, []HostStat{{Host: "api.example.com", Count: 4}}) {
		t.Fatalf("kind filter hosts = %+v", byKind.Hosts)
	}

	byHost := inv.Filter("", "cdn.example.com", "")
	if byHost.Total != 1 || byHost.Items[0].Kind != KindStatic {
		t.Fatalf("host filter: %+v", byHost.Items)
	}

	byQuery := inv.Filter("", "", "TOKEN") // case-insensitive URL substring
	if byQuery.Total != 2 {
		t.Fatalf("query filter total = %d, want 2: %+v", byQuery.Total, byQuery.Items)
	}

	combo := inv.Filter("api", "api.example.com", "upload")
	if combo.Total != 1 || combo.Items[0].URL != "https://api.example.com/v2/upload" {
		t.Fatalf("combined filter: %+v", combo.Items)
	}

	none := inv.Filter("static", "api.example.com", "")
	if none.Total != 0 || len(none.Items) != 0 || len(none.Hosts) != 0 || len(none.ByKind) != 0 {
		t.Fatalf("empty view: %+v", none)
	}

	all := inv.Filter("", "", "")
	if all.Total != 7 {
		t.Fatalf("no-op filter total = %d, want 7", all.Total)
	}

	// The filtered Items slice must be independent: appending to it must not
	// move the source inventory.
	api := inv.Filter("api", "", "")
	api.Items = append(api.Items, Asset{Kind: "ghost"})
	if inv.Total != 7 || len(inv.Items) != 7 {
		t.Fatalf("source inventory mutated: total=%d len=%d", inv.Total, len(inv.Items))
	}
	if api.Total != 4 || len(api.Items) != 5 {
		t.Fatalf("filtered view: total=%d len=%d", api.Total, len(api.Items))
	}
}
