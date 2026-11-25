package main

import (
	"bytes"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadLinks(t *testing.T) {
	files := t.TempDir()
	u := NewUploadLinks("https://wa.example.com/")
	mux := http.NewServeMux()
	mux.Handle("/upload/{token}", u)

	link, path, _, err := u.Create(files, "../Quarterly report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://wa.example.com/upload/") || path != "uploads/Quarterly report.pdf" {
		t.Fatalf("link %q path %q", link, path)
	}
	target := strings.TrimPrefix(link, "https://wa.example.com")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, target, strings.NewReader("%PDF-1.7 body")))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	got, err := os.ReadFile(filepath.Join(files, filepath.FromSlash(path)))
	if err != nil || string(got) != "%PDF-1.7 body" {
		t.Fatalf("stored file: %q %v", got, err)
	}
	// The stored file is readable by send_file's path input.
	if data, name, err := (&tools{filesDir: files}).fileInput(toolRequest(map[string]any{"path": path})); err != nil || name != "Quarterly report.pdf" || len(data) == 0 {
		t.Fatalf("fileInput: %q %v", name, err)
	}

	// Links are single use.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, target, strings.NewReader("again")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reused link: %d", rec.Code)
	}

	// Multipart uploads (curl -F) work too.
	link, path, _, _ = u.Create(files, "data.csv")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "whatever.csv")
	_, _ = fw.Write([]byte("a,b\n1,2\n"))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, strings.TrimPrefix(link, "https://wa.example.com"), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got, _ := os.ReadFile(filepath.Join(files, filepath.FromSlash(path))); rec.Code != http.StatusOK || string(got) != "a,b\n1,2\n" {
		t.Fatalf("multipart: %d %q", rec.Code, got)
	}

	// Expired links are refused.
	link, _, _, _ = u.Create(files, "late.pdf")
	u.now = func() time.Time { return time.Now().Add(uploadLinkTTL + time.Minute) }
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, strings.TrimPrefix(link, "https://wa.example.com"), strings.NewReader("x")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expired link: %d", rec.Code)
	}
	// Unknown tokens too.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/upload/nope", strings.NewReader("x")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d", rec.Code)
	}
}

func TestFetchURLRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer srv.Close()
	if _, _, _, err := fetchURL(t.Context(), srv.URL+"/x.pdf"); err == nil {
		t.Fatal("fetched from loopback")
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://example.com/x", "not a url"} {
		if _, _, _, err := fetchURL(t.Context(), bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for ip, want := range map[string]bool{"8.8.8.8": true, "10.0.0.1": false, "192.168.1.5": false, "169.254.169.254": false, "100.100.1.1": false, "::1": false, "2606:4700::1111": true} {
		if publicIP(net.ParseIP(ip)) != want {
			t.Errorf("publicIP(%s) != %v", ip, want)
		}
	}
}
