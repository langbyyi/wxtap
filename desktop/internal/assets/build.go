package assets

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// Build merges endpoints found in decompiled code (codeDir, skipped when
// empty — it is not an error on its own), captured traffic records and cloud
// function names into one deduplicated inventory. When none of the three
// sources is present it fails; a non-empty codeDir that cannot be read is an
// error too, even when records exist.
func Build(codeDir string, records []traffic.Record, cloudFns []string) (*Inventory, error) {
	c := newCollector()
	hasSource := false
	if codeDir != "" {
		drafts, _, err := collectFromDir(codeDir)
		if err != nil {
			return nil, err
		}
		for _, d := range drafts {
			c.add(d)
		}
		hasSource = true
	}
	if len(records) > 0 {
		hasSource = true
		for i := range records {
			if d := draftFromRecord(&records[i]); d != nil {
				c.add(d)
			}
		}
	}
	for _, name := range cloudFns {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		c.add(cloudDraft(name))
		hasSource = true
	}
	if !hasSource {
		return nil, errors.New("没有可用来源：代码目录为空，且没有流量记录或云函数")
	}
	return newInventory(c.assets()), nil
}

// cloudDraft gives a cloud function a cloudfunction:// pseudo URL so it
// survives dedup and export like any other asset; cloud functions have no
// on-the-wire endpoint of their own.
func cloudDraft(name string) *draft {
	p := parsedURL{
		scheme:  "cloudfunction",
		host:    "cloud",
		path:    name,
		display: "cloudfunction://" + name,
	}
	d := newDraft(KindCloud, "*", p)
	d.addHits(1)
	d.addSource(Source{Type: "code", Ref: "cloud"})
	return d
}

// assetID is fnv-1a64 over kind|method|scheme|host|path, rendered as 16 hex
// digits. The inputs are exactly the dedup dimensions plus kind, so the same
// endpoint yields the same ID on every run.
func assetID(kind, method, scheme, host, path string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(kind + "|" + method + "|" + scheme + "|" + host + "|" + path))
	return fmt.Sprintf("%016x", h.Sum64())
}
