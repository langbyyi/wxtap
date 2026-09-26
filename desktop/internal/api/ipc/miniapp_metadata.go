package ipc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxIconDataURLBytes = 256 << 10

// MiniAppMetadata contains only metadata recovered from the local package.
// IconPath is always relative to the decompiled output directory.
type MiniAppMetadata struct {
	IconPath       string `json:"icon_path"`
	IconDataURL    string `json:"icon_data_url"`
	IconConfidence string `json:"icon_confidence"`
	MetadataSource string `json:"metadata_source"`
}

// ReadMiniAppMetadata reads only an icon explicitly declared by local package
// metadata. It never guesses from arbitrary image files, performs network
// access, or executes package code; an unknown icon is safer than a wrong one.
func ReadMiniAppMetadata(outDir string) MiniAppMetadata {
	metadata := MiniAppMetadata{MetadataSource: "local_package"}
	if declared := declaredIconPath(outDir); declared != "" {
		if data, ok := readIcon(outDir, declared); ok {
			metadata.IconPath = declared
			metadata.IconConfidence = "declared"
			metadata.IconDataURL = iconDataURL(declared, data)
			return metadata
		}
	}
	return metadata
}

// ReadMiniAppCachedIcon reads the icon stored by WeChat under the exact AppID
// cache key. It avoids associating a similarly named image from another app.
func ReadMiniAppCachedIcon(userDir, appID string) MiniAppMetadata {
	metadata := MiniAppMetadata{MetadataSource: "wechat_cache"}
	if strings.TrimSpace(userDir) == "" || strings.TrimSpace(appID) == "" {
		return metadata
	}
	path := filepath.Join(userDir, "applet", "local", appID, "store", "images", appID)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > maxIconDataURLBytes {
		return metadata
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		return metadata
	}
	metadata.IconPath = filepath.ToSlash(filepath.Join("wechat-cache", appID))
	metadata.IconConfidence = "cached"
	metadata.IconDataURL = iconDataURL(metadata.IconPath, data)
	return metadata
}

func declaredIconPath(outDir string) string {
	for _, filename := range []string{"app-config.json", "app.json", "project.config.json"} {
		data, err := os.ReadFile(filepath.Join(outDir, filename))
		if err != nil {
			continue
		}
		var cfg map[string]any
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		for _, key := range []string{"iconPath", "icon", "logo", "avatar"} {
			value, ok := cfg[key].(string)
			if !ok || strings.TrimSpace(value) == "" || strings.Contains(value, "://") {
				continue
			}
			if _, ok := readIcon(outDir, value); ok {
				return filepath.ToSlash(filepath.Clean(value))
			}
		}
	}
	return ""
}

func readIcon(outDir, value string) ([]byte, bool) {
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return nil, false
	}
	root, err := filepath.Abs(outDir)
	if err != nil {
		return nil, false
	}
	target, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return nil, false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false
	}
	data, err := os.ReadFile(target)
	if err != nil || len(data) == 0 || len(data) > maxIconDataURLBytes {
		return nil, false
	}
	return data, true
}

func iconDataURL(path string, data []byte) string {
	if len(data) == 0 || len(data) > maxIconDataURLBytes {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(path))
	mimeType := mime.TypeByExtension(ext)
	if ext == ".webp" {
		mimeType = "image/webp"
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
		if mimeType == "application/octet-stream" {
			return ""
		}
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))
}
