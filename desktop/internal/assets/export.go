package assets

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Export renders the inventory in one of the following formats:
//
//   - "nuclei": every unique api/ws URL, one per line, sorted — the target
//     list for `nuclei -l`. Static resources and cloud pseudo URLs are not
//     scan targets.
//   - "httpx": every unique scheme://host reachable over HTTP, one per line,
//     sorted — the target list for `httpx -l`.
//   - "json": the whole inventory as compact JSON.
//   - "txt": every asset URL one per line (cloud included), item order.
//   - "csv": one row per asset with RFC4180 escaping.
func (inv *Inventory) Export(format string) (string, error) {
	switch format {
	case "nuclei":
		return inv.exportNuclei(), nil
	case "httpx":
		return inv.exportHTTPX(), nil
	case "json":
		return inv.exportJSON()
	case "txt":
		return inv.exportTXT(), nil
	case "csv":
		return inv.exportCSV()
	default:
		return "", fmt.Errorf("不支持的导出格式: %q（支持 nuclei/httpx/json/txt/csv）", format)
	}
}

func (inv *Inventory) exportNuclei() string {
	seen := make(map[string]bool)
	lines := make([]string, 0, len(inv.Items))
	for _, it := range inv.Items {
		if it.Kind != KindAPI && it.Kind != KindWS {
			continue
		}
		if seen[it.URL] {
			continue
		}
		seen[it.URL] = true
		lines = append(lines, it.URL)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (inv *Inventory) exportHTTPX() string {
	seen := make(map[string]bool)
	lines := make([]string, 0, len(inv.Items))
	for _, it := range inv.Items {
		scheme := urlScheme(it.URL)
		if scheme != "http" && scheme != "https" {
			continue
		}
		target := scheme + "://" + it.Host
		if seen[target] {
			continue
		}
		seen[target] = true
		lines = append(lines, target)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (inv *Inventory) exportJSON() (string, error) {
	b, err := json.Marshal(inv)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (inv *Inventory) exportTXT() string {
	seen := make(map[string]bool)
	lines := make([]string, 0, len(inv.Items))
	for _, it := range inv.Items {
		if seen[it.URL] {
			continue
		}
		seen[it.URL] = true
		lines = append(lines, it.URL)
	}
	return strings.Join(lines, "\n")
}

func (inv *Inventory) exportCSV() (string, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"kind", "method", "host", "path", "url", "hits", "source_count"}); err != nil {
		return "", err
	}
	for _, it := range inv.Items {
		row := []string{
			it.Kind,
			it.Method,
			it.Host,
			it.Path,
			it.URL,
			strconv.Itoa(it.Hits),
			strconv.Itoa(len(it.Sources)),
		}
		if err := w.Write(row); err != nil {
			return "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// urlScheme returns the lowercased scheme of raw, or "" when it does not
// parse (cloudfunction:// pseudo URLs parse like any absolute URL).
func urlScheme(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme)
}
