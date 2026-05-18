package main

import (
	"bytes"
	"os"
	"testing"
)

func TestGeneratedManifestInSync(t *testing.T) {
	generated, err := renderManifest()
	if err != nil {
		t.Fatalf("生成 plugin.yaml 失败: %v", err)
	}

	manifestPath, err := manifestFilePath()
	if err != nil {
		t.Fatalf("定位 plugin.yaml 失败: %v", err)
	}

	current, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读取 plugin.yaml 失败: %v", err)
	}

	if !bytes.Equal(generated, current) {
		t.Fatalf("plugin.yaml 与运行时元信息不同步，请执行: go run ./cmd/genmanifest")
	}
}
