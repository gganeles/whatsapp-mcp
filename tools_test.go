package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func toolRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func TestFileInputSandbox(t *testing.T) {
	base := t.TempDir()
	files := filepath.Join(base, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "whatsapp.db")
	_ = os.WriteFile(secret, []byte("keys"), 0o600)
	_ = os.WriteFile(filepath.Join(files, "photo.jpg"), []byte("jpeg"), 0o600)
	_ = os.Symlink(secret, filepath.Join(files, "link.db"))
	tl := &tools{filesDir: files}

	if data, name, err := tl.fileInput(toolRequest(map[string]any{"path": "photo.jpg"})); err != nil || string(data) != "jpeg" || name != "photo.jpg" {
		t.Fatalf("relative path: %q %q %v", data, name, err)
	}
	if _, _, err := tl.fileInput(toolRequest(map[string]any{"path": filepath.Join(files, "photo.jpg")})); err != nil {
		t.Fatalf("absolute path inside files dir: %v", err)
	}
	for _, path := range []string{"../whatsapp.db", secret, "link.db", "/etc/passwd"} {
		if _, _, err := tl.fileInput(toolRequest(map[string]any{"path": path})); err == nil {
			t.Errorf("path %q escaped the files folder", path)
		}
	}

	data, name, err := tl.fileInput(toolRequest(map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString([]byte("hi")), "file_name": "../../notes.txt"}))
	if err != nil || string(data) != "hi" || name != "notes.txt" {
		t.Fatalf("base64 input: %q %q %v", data, name, err)
	}
}

func TestSanitizeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":         "report.pdf",
		"../../etc/passwd":   "passwd",
		"..hidden":           "hidden",
		"a:b*c?.txt":         "a_b_c_.txt",
		"":                   "file",
		"123@s.whatsapp.net": "123@s.whatsapp.net",
	} {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
