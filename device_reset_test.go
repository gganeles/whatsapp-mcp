package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// After WhatsApp removes the device, the tools must explain what happened and pairing
// must start from a fresh device instead of failing with "invalid use of deleted device".
func TestRemovedDeviceIsReplaced(t *testing.T) {
	ctx := context.Background()
	wa, err := NewWhatsApp(ctx, t.TempDir(), "15550001111", waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	defer wa.Close()
	if err = wa.Store.UpsertChat(ctx, "15550002222@s.whatsapp.net", "Bob", false, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Pretend the device was paired, then removed the way whatsmeow handles device_removed.
	old := wa.Client()
	id := types.NewADJID("15550001111", 0, 3)
	old.Store.ID = &id
	old.Store.Account = &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err = old.Store.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if !wa.LoggedIn() {
		t.Fatal("expected to be linked")
	}
	wa.handleEvent(old, &events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	if err = old.Store.Delete(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for wa.Client() == old && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	fresh := wa.Client()
	if fresh == old || fresh.Store.Deleted || fresh.Store.ID != nil {
		t.Fatal("expected a fresh, unpaired device after the logout")
	}
	if wa.LoggedIn() {
		t.Fatal("should not be linked after removal")
	}

	// A late duplicate event from the old client must not replace the fresh one.
	wa.resetDevice(old)
	if wa.Client() != fresh {
		t.Fatal("stale reset replaced the fresh client")
	}

	tl := &tools{wa: wa, filesDir: wa.FilesDir}
	err = tl.requireConnected()
	if err == nil || !strings.Contains(err.Error(), "removed this linked device") {
		t.Fatalf("requireConnected: %v", err)
	}
	res, _ := tl.searchContacts(ctx, toolRequest(map[string]any{"query": "bob"}))
	if text, _ := res.Content[0].(mcp.TextContent); !res.IsError || !strings.Contains(text.Text, "pair_device") {
		t.Fatalf("search_contacts while unlinked: %+v", res)
	}
	// whatsmeow's raw error gets a message that says what to do.
	res, _ = errResult(fmt.Errorf("send: %w", store.ErrDeviceDeleted))
	if text, _ := res.Content[0].(mcp.TextContent); !strings.Contains(text.Text, "pair_device") {
		t.Fatalf("deleted device error: %q", text.Text)
	}
	if chats, _, _ := wa.Store.Stats(ctx); chats != 1 {
		t.Fatalf("stored chats lost: %d", chats)
	}
}
