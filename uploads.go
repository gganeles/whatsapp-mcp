package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxSendFileSize = 100 << 20
	uploadLinkTTL   = 30 * time.Minute
	uploadsSubdir   = "uploads"
)

// UploadLinks hands out short-lived, single-use URLs that a client can PUT or POST a file
// to. The file lands in the account's files folder, where send_file can then send it. This
// lets a model send a file from its own sandbox (e.g. with curl) without inlining base64.
type UploadLinks struct {
	baseURL string

	mu    sync.Mutex
	slots map[string]*uploadSlot
	now   func() time.Time
}

type uploadSlot struct {
	filesDir string
	name     string
	expires  time.Time
}

func NewUploadLinks(publicURL string) *UploadLinks {
	return &UploadLinks{baseURL: strings.TrimRight(publicURL, "/"), slots: map[string]*uploadSlot{}, now: time.Now}
}

// Create returns a new upload URL for a file called name, and the path send_file will
// find it under once it has been uploaded.
func (u *UploadLinks) Create(filesDir, name string) (link, path string, expires time.Time, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	name = sanitizeFileName(name)
	expires = u.now().Add(uploadLinkTTL)

	u.mu.Lock()
	defer u.mu.Unlock()
	for k, s := range u.slots {
		if u.now().After(s.expires) {
			delete(u.slots, k)
		}
	}
	u.slots[token] = &uploadSlot{filesDir: filesDir, name: name, expires: expires}
	return u.baseURL + "/upload/" + token, uploadsSubdir + "/" + name, expires, nil
}

// take removes and returns the slot for a token, if it exists and hasn't expired.
func (u *UploadLinks) take(token string) *uploadSlot {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.slots[token]
	delete(u.slots, token)
	if s == nil || u.now().After(s.expires) {
		return nil
	}
	return s
}

// ServeHTTP accepts the file body for PUT/POST /upload/{token}. The body is either the raw
// file bytes (curl -T file, or --data-binary @file) or a multipart form with one file field
// (curl -F file=@file).
func (u *UploadLinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		w.Header().Set("Allow", "PUT, POST")
		http.Error(w, "use PUT or POST", http.StatusMethodNotAllowed)
		return
	}
	slot := u.take(r.PathValue("token"))
	if slot == nil {
		http.Error(w, "upload link is unknown, used, or expired; create a new one", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSendFileSize+1<<20)

	var body io.Reader = r.Body
	if mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				http.Error(w, "no file in the multipart form", http.StatusBadRequest)
				return
			}
			if part.FileName() != "" {
				body = part
				break
			}
		}
	}

	rel := filepath.Join(uploadsSubdir, slot.name)
	n, err := writeFileFrom(slot.filesDir, rel, io.LimitReader(body, maxSendFileSize+1))
	if err == nil && n > maxSendFileSize {
		err = errors.New("file is larger than 100 MB")
	}
	if err != nil {
		if root, rerr := os.OpenRoot(slot.filesDir); rerr == nil {
			_ = root.Remove(rel)
			root.Close()
		}
		http.Error(w, "upload failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uploaded": true, "path": filepath.ToSlash(rel), "bytes": n,
		"next": "Call send_file with this path.",
	})
}

// writeFileFrom streams r into rel inside filesDir, which it can't escape.
func writeFileFrom(filesDir, rel string, r io.Reader) (int64, error) {
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	if err = root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
		return 0, err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// fetchURL downloads a file from a public http(s) URL. Connections to loopback, private,
// and link-local addresses are refused so the tool can't be used to probe the server's
// own network.
func fetchURL(ctx context.Context, raw string) (data []byte, name, contentType string, err error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", "", errors.New("url must be an http:// or https:// URL")
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !publicIP(ip) {
			return fmt.Errorf("refusing to fetch from non-public address %s", host)
		}
		return nil
	}}
	client := &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 15 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return errors.New("redirect to a non-http URL")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", "whatsapp-mcp")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("download failed: %s", resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxSendFileSize+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("download failed: %w", err)
	}
	if len(data) > maxSendFileSize {
		return nil, "", "", errors.New("file is larger than 100 MB")
	}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	} else {
		name = filepath.Base(resp.Request.URL.Path)
	}
	if name == "" || name == "/" || name == "." {
		name = "file"
	}
	return data, sanitizeFileName(name), resp.Header.Get("Content-Type"), nil
}

func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10), used by Tailscale and some VPNs.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return false
	}
	return true
}
