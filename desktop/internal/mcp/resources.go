package mcp

// resources 面：把 skills/ 的技能文档作为 MCP 资源暴露。相比只能靠
// miniapp_get_skills 工具拉取，资源让客户端的资源浏览器直接发现、按名读取。
// URI 约定 skill://<文件名>，一一对应 SkillsDir 下的 .md 文件。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skillURIScheme is the URI scheme of the skill resources.
const skillURIScheme = "skill://"

// wxtapURIScheme is the scheme of the built-in reference resources.
const wxtapURIScheme = "wxtap://reference/"

type resourceDescriptor struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
	Size        int    `json:"size"`
}

// referenceResources are the built-in scheme documents: stable knowledge an
// agent would otherwise have to reverse-engineer from tool outputs (the hook
// record shapes, the engine failure codes).
func referenceResources() []resourceDescriptor {
	out := make([]resourceDescriptor, 0, len(referenceBodies))
	for uri, body := range referenceBodies {
		out = append(out, resourceDescriptor{
			URI:         wxtapURIScheme + uri,
			Name:        uri,
			Description: firstHeading([]byte(body)),
			MimeType:    "text/markdown",
			Size:        len(body),
		})
	}
	return out
}

// listResources describes every skill file as a readable resource, plus the
// built-in reference documents. A missing or empty skills directory is an
// empty skill list, not an error: the surface is additive and must not make
// resources/list fail where miniapp_get_skills answers "暂无 skill 文件".
func (s *Server) listResources() []resourceDescriptor {
	files := skillFiles(s.deps.SkillsDir)
	resources := make([]resourceDescriptor, 0, len(files)+len(referenceBodies))
	resources = append(resources, referenceResources()...)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		name := filepath.Base(file)
		resources = append(resources, resourceDescriptor{
			URI:         skillURIScheme + name,
			Name:        strings.TrimSuffix(name, filepath.Ext(name)),
			Description: firstHeading(data),
			MimeType:    "text/markdown",
			Size:        len(data),
		})
	}
	return resources
}

// readResource resolves one resource URI. Skill URIs read their file; the
// wxtap:// scheme serves the built-in references; anything else is a plain
// "not found" — a guessed URI must not read as empty content.
func (s *Server) readResource(req *request) *response {
	var params struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(req.Params, &params) != nil || params.URI == "" {
		return fail(req, &rpcError{Code: -32602, Message: "resources/read requires a uri"})
	}
	switch {
	case strings.HasPrefix(params.URI, wxtapURIScheme):
		name := strings.TrimPrefix(params.URI, wxtapURIScheme)
		body, known := referenceBodies[name]
		if !known {
			return fail(req, &rpcError{Code: -32602, Message: "resource not found: " + params.URI})
		}
		return textResource(req, params.URI, body)
	case strings.HasPrefix(params.URI, skillURIScheme):
		name := filepath.Base(strings.TrimPrefix(params.URI, skillURIScheme))
		data, err := os.ReadFile(filepath.Join(s.deps.SkillsDir, name))
		if err != nil {
			return fail(req, &rpcError{Code: -32602, Message: "resource not found: " + params.URI})
		}
		return textResource(req, params.URI, string(data))
	default:
		return fail(req, &rpcError{Code: -32602, Message: "unknown resource scheme: " + params.URI})
	}
}

func textResource(req *request, uri string, text string) *response {
	return ok(req, map[string]any{
		"contents": []map[string]any{{
			"uri":      uri,
			"mimeType": "text/markdown",
			"text":     text,
		}},
	})
}

// skillFiles lists the readable skill markdown files, README excluded (it
// documents the directory itself, it is not a skill).
func skillFiles(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.EqualFold(entry.Name(), "README.md") || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)
	return files
}

// firstHeading is the resource description: the first markdown heading, or
// failing that the first non-empty line, capped for list payloads.
func firstHeading(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		text = strings.TrimLeft(text, "#")
		text = strings.TrimSpace(text)
		if text != "" {
			return truncateText(text, 100)
		}
	}
	return ""
}
