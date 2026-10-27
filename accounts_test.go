package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	alicePhone, alicePassword = "14155550001", "alice password 1"
	bobPhone, bobPassword     = "14155550002", "bob password 22"
)

// mcpSession is a minimal streamable-HTTP MCP client for tests.
type mcpSession struct {
	ot        *oauthTest
	token, id string
	nextID    int
}

func (ot *oauthTest) openSession(token string) *mcpSession {
	ms := &mcpSession{ot: ot, token: token}
	resp, _ := ms.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	ms.id = resp.Header.Get("Mcp-Session-Id")
	ms.notify("notifications/initialized")
	return ms
}

func (ms *mcpSession) post(body any) (*http.Response, string) {
	data, _ := json.Marshal(body)
	headers := []string{"Accept", "application/json, text/event-stream", "Authorization", "Bearer " + ms.token}
	if ms.id != "" {
		headers = append(headers, "Mcp-Session-Id", ms.id)
	}
	return ms.ot.do("POST", "/mcp", strings.NewReader(string(data)), "application/json", headers...)
}

func (ms *mcpSession) notify(method string) {
	ms.post(map[string]any{"jsonrpc": "2.0", "method": method})
}

func (ms *mcpSession) rpc(method string, params any) (*http.Response, string) {
	ms.nextID++
	return ms.post(map[string]any{"jsonrpc": "2.0", "id": ms.nextID, "method": method, "params": params})
}

// callTool returns the text of a tool result.
func (ms *mcpSession) callTool(name string, args map[string]any) string {
	ms.ot.t.Helper()
	resp, body := ms.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if resp.StatusCode != 200 {
		ms.ot.t.Fatalf("%s: HTTP %d %s", name, resp.StatusCode, body)
	}
	// The body is either plain JSON or a single SSE event.
	if i := strings.Index(body, "data: "); i >= 0 {
		body = strings.TrimSpace(body[i+len("data: "):])
	}
	var out struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		ms.ot.t.Fatalf("%s: bad response %q: %v", name, body, err)
	}
	var texts []string
	for _, c := range out.Result.Content {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n")
}

func TestAccountsAreIsolated(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := OpenAccountStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for phone, pw := range map[string]string{alicePhone: alicePassword, bobPhone: bobPassword} {
		if err = store.AddAccount(ctx, phone, pw); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewAccountManager(dataDir, store, waLog.Noop)
	t.Cleanup(manager.Close)

	// Give each account a chat and a message only it should see.
	for phone, secret := range map[string]string{alicePhone: "alice-secret", bobPhone: "bob-secret"} {
		tl, err := manager.Tools(ctx, phone)
		if err != nil {
			t.Fatal(err)
		}
		chat := fmt.Sprintf("%s@s.whatsapp.net", strings.Repeat("9", 11))
		now := time.Now()
		if err = tl.wa.Store.UpsertChat(ctx, chat, "Chat of "+secret, false, now); err != nil {
			t.Fatal(err)
		}
		if err = tl.wa.Store.SaveMessage(ctx, &Message{ChatJID: chat, ID: "m-" + secret, Timestamp: now, Type: "text", Text: "the " + secret}); err != nil {
			t.Fatal(err)
		}
	}

	// Opening an account creates its session database but doesn't link it.
	if IsLinked(ctx, AccountDir(dataDir, alicePhone)) {
		t.Fatal("unpaired account reported as linked")
	}

	resolve := func(ctx context.Context) (*tools, error) {
		id, ok := AccountFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("not signed in")
		}
		return manager.Tools(ctx, id)
	}
	ot := newOAuthTestWith(t, store, NewMCPServer(resolve, "test", false))

	// One account's password doesn't open another account.
	_, client := ot.register(claudeRedirect)
	_, challenge := pkce()
	if resp := ot.authorizeAs(bobPhone, client["client_id"].(string), claudeRedirect, challenge, alicePassword); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Alice's password opened Bob's account: %d", resp.StatusCode)
	}

	alice := ot.openSession(ot.signIn("+1 415 555 0001", alicePassword))
	bob := ot.openSession(ot.signIn(bobPhone, bobPassword))

	for _, tc := range []struct {
		s           *mcpSession
		own, others string
	}{{alice, "alice-secret", "bob-secret"}, {bob, "bob-secret", "alice-secret"}} {
		chats := tc.s.callTool("list_chats", nil)
		found := tc.s.callTool("search_messages", map[string]any{"query": "secret"})
		status := tc.s.callTool("get_status", nil)
		if !strings.Contains(chats, tc.own) || strings.Contains(chats, tc.others) {
			t.Errorf("list_chats leaked across accounts: %s", chats)
		}
		if !strings.Contains(found, tc.own) || strings.Contains(found, tc.others) {
			t.Errorf("search_messages leaked across accounts: %s", found)
		}
		if strings.Contains(status, "data_dir") {
			t.Errorf("get_status exposes server paths: %s", status)
		}
	}

	// Reusing another account's MCP session ID doesn't grant access to its data:
	// the account always comes from the bearer token.
	hijack := &mcpSession{ot: ot, token: bob.token, id: alice.id, nextID: 10}
	if got := hijack.callTool("list_chats", nil); strings.Contains(got, "alice-secret") {
		t.Fatalf("session hijack exposed Alice's chats: %s", got)
	}

	// Removing an account signs it out immediately.
	if err = store.RemoveAccount(ctx, bobPhone); err != nil {
		t.Fatal(err)
	}
	if resp, _ := bob.rpc("tools/list", map[string]any{}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed account still signed in: %d", resp.StatusCode)
	}
	if got := alice.callTool("list_chats", nil); !strings.Contains(got, "alice-secret") {
		t.Fatalf("removing Bob affected Alice: %s", got)
	}
}

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"+1 (415) 555-1234": "14155551234",
		"447700900123":      "447700900123",
		"0123456789":        "",
		"+1 415 abc":        "",
		"123":               "",
		"../../etc":         "",
	} {
		got, err := NormalizePhone(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
