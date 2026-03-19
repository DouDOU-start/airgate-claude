package gateway

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed webdist/*
var webDistFS embed.FS

// GetWebAssets 实现 sdk.WebAssetsProvider 接口
// 开发模式优先读取磁盘上的 web/dist，生产模式回退到嵌入资源。
func (g *AnthropicGateway) GetWebAssets() map[string][]byte {
	if assets := loadDevWebAssets(); len(assets) > 0 {
		return assets
	}

	assets := make(map[string][]byte)
	if err := fs.WalkDir(webDistFS, "webdist", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		content, err := webDistFS.ReadFile(path)
		if err != nil {
			return nil
		}
		relPath := strings.TrimPrefix(path, "webdist/")
		assets[relPath] = content
		return nil
	}); err != nil && g != nil && g.logger != nil {
		g.logger.Warn("读取嵌入前端资源失败", "error", err)
	}
	return assets
}

func loadDevWebAssets() map[string][]byte {
	candidates := []string{
		filepath.Join("..", "web", "dist"),
		filepath.Join("web", "dist"),
	}

	for _, dir := range candidates {
		assets := loadAssetsFromDir(dir)
		if len(assets) > 0 {
			return assets
		}
	}

	return nil
}

func loadAssetsFromDir(root string) map[string][]byte {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	assets := make(map[string][]byte)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}

		assets[filepath.ToSlash(relPath)] = content
		return nil
	})

	if len(assets) == 0 {
		return nil
	}
	return assets
}
