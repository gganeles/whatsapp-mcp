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

func TestDetectMimeAndKind(t *testing.T) {
	cases := []struct {
		name, hint, mime, kind string
		data                   []byte
	}{
		{"report.pdf", "", "application/pdf", "document", []byte("%PDF-1.7")},
		{"notes.docx", "", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "document", []byte("PK\x03\x04")},
		{"sheet.xlsx", "", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "document", []byte("PK\x03\x04")},
		{"archive.zip", "", "application/zip", "document", []byte("PK\x03\x04")},
		{"readme.txt", "", "text/plain", "document", []byte("hello")},
		{"photo.JPG", "", "image/jpeg", "image", []byte{0xff, 0xd8, 0xff}},
		{"anim.gif", "", "image/gif", "document", []byte("GIF89a")},
		{"clip.mp4", "", "video/mp4", "video", nil},
		{"clip.mkv", "", "video/x-matroska", "document", nil},
		{"song.mp3", "", "audio/mpeg", "audio", nil},
		{"take.wav", "", "audio/wav", "document", nil},
		{"voice.opus", "", "audio/ogg", "audio", nil},
		{"download", "application/pdf; charset=binary", "application/pdf", "document", []byte("x")},
		{"noext", "", "application/pdf", "document", []byte("%PDF-1.4\n")},
	}
	for _, c := range cases {
		mt := detectMimeType(c.name, c.data, c.hint)
		if mt != c.mime || autoKind(mt) != c.kind {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, mt, autoKind(mt), c.mime, c.kind)
		}
	}
}
