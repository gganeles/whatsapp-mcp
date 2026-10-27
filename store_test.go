package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMessageStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertChat(ctx, "111@s.whatsapp.net", "Alice", false, base))
	must(s.UpsertChat(ctx, "222@g.us", "Hiking 100%", true, base.Add(time.Hour)))
	must(s.UpsertChat(ctx, "111@s.whatsapp.net", "", false, base.Add(-time.Hour))) // must not clobber name or time

	for i, text := range []string{"hello there", "lunch tomorrow?", "see you at lunch"} {
		must(s.SaveMessage(ctx, &Message{ChatJID: "111@s.whatsapp.net", ID: string(rune('a' + i)), SenderJID: "111@s.whatsapp.net",
			SenderName: "Alice", Timestamp: base.Add(time.Duration(i) * time.Minute), Type: "text", Text: text}))
	}
	must(s.SaveMessage(ctx, &Message{ChatJID: "222@g.us", ID: "g1", FromMe: true, Timestamp: base.Add(time.Hour), Type: "image", Text: "summit"}))

	chats, err := s.ListChats(ctx, "", false, false, 10, 0)
	must(err)
	if len(chats) != 2 || chats[0].JID != "222@g.us" || chats[1].Name != "Alice" || chats[1].MessageCount != 3 {
		t.Fatalf("unexpected chats: %+v", chats)
	}
	if chats[0].LastSender != "me" || chats[1].LastMessage != "see you at lunch" {
		t.Fatalf("unexpected last message: %+v", chats)
	}
	if c, _ := s.ListChats(ctx, "100%", false, false, 10, 0); len(c) != 1 {
		t.Fatalf("LIKE escaping broken: %+v", c)
	}

	hits, err := s.Messages(ctx, MessageFilter{Query: "LUNCH", Limit: 10})
	must(err)
	if len(hits) != 2 || hits[0].ID != "c" {
		t.Fatalf("unexpected search hits: %+v", hits)
	}
	hits, _ = s.Messages(ctx, MessageFilter{Query: "lunch tomorrow", Limit: 10})
	if len(hits) != 1 || hits[0].ID != "b" {
		t.Fatalf("multi-word search: %+v", hits)
	}

	must(s.EditMessage(ctx, "111@s.whatsapp.net", "a", "hello (edited)"))
	must(s.SaveMessage(ctx, &Message{ChatJID: "111@s.whatsapp.net", ID: "a", Timestamp: base, Type: "text", Text: "hello there"}))
	m, _ := s.GetMessage(ctx, "111@s.whatsapp.net", "a")
	if m.Text != "hello (edited)" || !m.Edited {
		t.Fatalf("edit lost on resync: %+v", m)
	}

	target, _ := s.FindMessage(ctx, "b")
	around, err := s.Context(ctx, target, 5, 5)
	must(err)
	if len(around) != 3 || around[0].ID != "a" || around[2].ID != "c" {
		t.Fatalf("unexpected context: %+v", around)
	}

	out := formatMessages(around, false)
	if !strings.Contains(out, "Alice: lunch tomorrow?  {id:b}") {
		t.Fatalf("unexpected format:\n%s", out)
	}
}

func TestBuildMessage(t *testing.T) {
	m := buildMessage(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("cat"), Mimetype: proto.String("image/jpeg")}})
	if m.Type != "image" || m.Text != "cat" || m.MediaType != "image" || len(m.Raw) == 0 {
		t.Fatalf("unexpected image message: %+v", m)
	}
	if buildMessage(&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}) != nil {
		t.Fatal("reactions should be skipped")
	}
	reply := buildMessage(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("yes"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("XYZ")}}})
	if reply.Text != "yes" || reply.QuotedID != "XYZ" {
		t.Fatalf("unexpected reply: %+v", reply)
	}
}
