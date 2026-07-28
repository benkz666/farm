package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGenConfigDeterministicAndAnchors(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	goA := filepath.Join(dir, "a", "gen_crops.go")
	jsA := filepath.Join(dir, "a", "crops.js")
	goB := filepath.Join(dir, "b", "gen_crops.go")
	jsB := filepath.Join(dir, "b", "crops.js")

	runGen(t, root, goA, jsA)
	runGen(t, root, goB, jsB)

	assertFileEqual(t, goA, goB)
	assertFileEqual(t, jsA, jsB)

	goBytes, err := os.ReadFile(goA)
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, err := os.ReadFile(jsA)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"DO NOT EDIT",
		"CropCount = 29",
		`Slug: "pingguo"`,
		`Slug: "xiaomai"`,
		`Name: "苹果"`,
		`Name: "小麦"`,
		`CycleMinutes: 2100`, // 草莓
		`CycleMinutes: 3540`, // 橙子
	} {
		if !bytes.Contains(goBytes, []byte(needle)) {
			t.Fatalf("generated Go missing %q", needle)
		}
	}
	// Go 表按 ID 索引：第 4 项（下标 3 的那一行）必须是苹果
	if !bytes.Contains(goBytes, []byte(`{ID: 4, Slug: "pingguo"`)) {
		t.Fatalf("Go cropTable must keep numeric id 4 = pingguo")
	}
	if !bytes.Contains(goBytes, []byte(`{ID: 15, Slug: "xiaomai"`)) {
		t.Fatalf("Go cropTable must assign xiaomai to numeric id 15")
	}

	for _, needle := range []string{
		"DO NOT EDIT",
		`id: "xiaomai"`,
		`id: "pingguo"`,
		`4: "pingguo"`,
		`15: "xiaomai"`,
		`pingguo: 4`,
		`xiaomai: 15`,
		`cycleMinutes: 2100`,
		`cycleMinutes: 3540`,
	} {
		if !bytes.Contains(jsBytes, []byte(needle)) {
			t.Fatalf("generated JS missing %q", needle)
		}
	}
	// 展示顺序：小麦出现在苹果之前（CSV 行序）
	xi := bytes.Index(jsBytes, []byte(`id: "xiaomai"`))
	pi := bytes.Index(jsBytes, []byte(`id: "pingguo"`))
	if xi < 0 || pi < 0 || xi > pi {
		t.Fatalf("JS CROPS display order must list xiaomai before pingguo (got xi=%d pi=%d)", xi, pi)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func runGen(t *testing.T, root, outGo, outJS string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(outGo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(outJS), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", ".", "-root", root, "-out-go", outGo, "-out-js", outJS)
	cmd.Dir = filepath.Join(root, "tools", "gen-config")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gen-config failed: %v\n%s", err, out)
	}
}

func assertFileEqual(t *testing.T, a, b string) {
	t.Helper()
	ba, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ba, bb) {
		t.Fatalf("generated outputs differ:\n  %s\n  %s", a, b)
	}
}
