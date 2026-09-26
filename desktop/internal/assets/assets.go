// Package assets builds a miniapp interface asset inventory: it extracts
// http(s)/ws(s) endpoints from decompiled source files and from captured
// traffic records, deduplicates them into stable assets, and exports the
// result in the target-list formats security scanners consume (nuclei,
// httpx) plus json/txt/csv.
package assets

import (
	"sort"
	"strings"
	"time"
)

// Asset kinds, as stored in Asset.Kind.
const (
	// KindAPI marks an endpoint that is neither a websocket nor a known
	// static resource extension.
	KindAPI = "api"
	// KindStatic marks a URL pointing at a media/font/script resource.
	KindStatic = "static"
	// KindWS marks a ws:// or wss:// endpoint.
	KindWS = "ws"
	// KindCloud marks a cloud function (cloudfunction:// pseudo URL).
	KindCloud = "cloud"
)

// Source records where an asset was observed. Type is "code" (Ref is the
// file path relative to the scanned root, or the literal "cloud" for cloud
// functions), "traffic" (Ref is the traffic record ID), or "more" — a
// synthetic marker whose Ref holds "+N" for the N sources dropped by the
// per-asset source cap.
type Source struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// Asset is one deduplicated endpoint. ID is fnv-1a64 over
// kind|method|scheme|host|path (hex), so the same endpoint yields the same ID
// across runs. URL is the display form: fragment stripped, host lowercased,
// query reduced to its keys ("?a&b"). A Method of "*" means the endpoint is
// only known from code or cloud functions, so any method applies.
type Asset struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Method    string    `json:"method"`
	Sources   []Source  `json:"sources"`
	Hits      int       `json:"hits"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	Tags      []string  `json:"tags,omitempty"`
	// TrafficSeen marks endpoints observed in captured traffic. Hits cannot
	// carry this meaning (code matches bump hits too), and the per-asset
	// source cap may drop traffic sources from the visible list, so the
	// verdict needs a dedicated field.
	TrafficSeen bool `json:"trafficSeen"`
}

// HostStat counts how many assets resolve to one host.
type HostStat struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

// Inventory is the merged asset list plus the aggregates the UI shows.
// Items are sorted by Host, then Path; Hosts by descending Count.
type Inventory struct {
	Items  []Asset        `json:"items"`
	Total  int            `json:"total"`
	ByKind map[string]int `json:"byKind"`
	Hosts  []HostStat     `json:"hosts"`
}

// Filter returns a view of the inventory narrowed by kind (exact match),
// host (exact match) and query (case-insensitive URL substring). Empty
// arguments are ignored. The returned Inventory is self-consistent:
// Total, ByKind and Hosts describe the filtered items, and its Items slice is
// independent from the source inventory (appending to it cannot move the
// original).
func (inv *Inventory) Filter(kind, host, query string) *Inventory {
	needle := strings.ToLower(query)
	items := make([]Asset, 0, len(inv.Items))
	for _, it := range inv.Items {
		if kind != "" && it.Kind != kind {
			continue
		}
		if host != "" && it.Host != host {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(it.URL), needle) {
			continue
		}
		items = append(items, it)
	}
	// Cap the slice so later appends allocate instead of writing past len
	// into the array shared with the source inventory.
	items = items[:len(items):len(items)]
	return newInventory(items)
}

// newInventory sorts items in place and computes Total/ByKind/Hosts for them.
func newInventory(items []Asset) *Inventory {
	sortAssets(items)
	byKind := make(map[string]int)
	hostCount := make(map[string]int)
	for _, it := range items {
		byKind[it.Kind]++
		hostCount[it.Host]++
	}
	hosts := make([]HostStat, 0, len(hostCount))
	for h, n := range hostCount {
		hosts = append(hosts, HostStat{Host: h, Count: n})
	}
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].Count != hosts[j].Count {
			return hosts[i].Count > hosts[j].Count
		}
		return hosts[i].Host < hosts[j].Host
	})
	return &Inventory{Items: items, Total: len(items), ByKind: byKind, Hosts: hosts}
}

func sortAssets(items []Asset) {
	sort.Slice(items, func(i, j int) bool { return lessAsset(items[i], items[j]) })
}

func lessAsset(a, b Asset) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Method != b.Method {
		return a.Method < b.Method
	}
	return a.URL < b.URL
}
