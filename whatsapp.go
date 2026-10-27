package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// WhatsApp wraps a whatsmeow client and mirrors everything it receives into the MessageStore.
type WhatsApp struct {
	Client  *whatsmeow.Client
	Store   *MessageStore
	DataDir string
	log     waLog.Logger

	mu        sync.Mutex
	pairCode  string // phone pairing code while pairing, empty otherwise
	qrCode    string // latest QR code while pairing, empty otherwise
	qrPath    string
	pairErr   error
	pairing   bool
	syncCount int // history sync batches processed since start
}

func NewWhatsApp(ctx context.Context, dataDir string, log waLog.Logger) (*WhatsApp, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	// Ask for as much history as the phone is willing to share when pairing.
	store.DeviceProps.RequireFullSync = proto.Bool(true)
	store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(3650)
	store.DeviceProps.HistorySyncConfig.FullSyncSizeMbLimit = proto.Uint32(10240)
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	store.SetOSInfo("WhatsApp MCP", [3]uint32{0, 1, 0})

	container, err := sqlstore.New(ctx, "sqlite3",
		"file:"+filepath.Join(dataDir, "whatsapp.db")+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000",
		log.Sub("Database"))
	if err != nil {
		return nil, fmt.Errorf("failed to open session database: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load device: %w", err)
	}
	msgStore, err := OpenMessageStore(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %w", err)
	}
	wa := &WhatsApp{
		Client:  whatsmeow.NewClient(device, log.Sub("Client")),
		Store:   msgStore,
		DataDir: dataDir,
		qrPath:  filepath.Join(dataDir, "login-qr.png"),
		log:     log,
	}
	wa.Client.AddEventHandler(wa.handleEvent)
	return wa, nil
}

func (wa *WhatsApp) LoggedIn() bool {
	return wa.Client.Store.ID != nil
}

// Connect connects to WhatsApp using the stored session. It fails if the device isn't paired yet.
func (wa *WhatsApp) Connect() error {
	if !wa.LoggedIn() {
		return errors.New("not paired with a phone yet")
	}
	return wa.Client.Connect()
}

// StartPairing links this device to a WhatsApp account. With a phone number (international
// format, digits only) it returns an 8-character pairing code to type into WhatsApp on the
// phone under Linked devices > Link with phone number instead. A QR code is generated
// either way and kept as a fallback; onQR is called for every new QR code.
//
// Pairing stays open for about 160 seconds, after which StartPairing has to be called again.
func (wa *WhatsApp) StartPairing(ctx context.Context, phone string, onQR func(code string)) (pairCode string, err error) {
	if wa.LoggedIn() {
		return "", errors.New("already paired; log out first to link a different account")
	}
	wa.mu.Lock()
	if wa.pairing {
		code := wa.pairCode
		wa.mu.Unlock()
		if phone != "" && code != "" {
			return code, nil
		}
		if phone == "" {
			return "", nil
		}
		// A QR-only pairing is running; restart it so we can request a code.
		wa.Client.Disconnect()
		wa.waitPairingDone(5 * time.Second)
		wa.mu.Lock()
	}
	wa.pairing = true
	wa.pairErr = nil
	wa.pairCode = ""
	wa.mu.Unlock()

	fail := func(err error) (string, error) {
		wa.mu.Lock()
		wa.pairing = false
		wa.pairErr = err
		wa.mu.Unlock()
		return "", err
	}
	// The QR channel needs a context that outlives this call; the channel closes by itself
	// when pairing succeeds or times out.
	qrChan, err := wa.Client.GetQRChannel(context.Background())
	if err != nil {
		return fail(err)
	}
	if err = wa.Client.Connect(); err != nil {
		return fail(err)
	}
	ready := make(chan struct{})
	go wa.consumeQR(qrChan, ready, onQR)

	select {
	case <-ready:
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-time.After(30 * time.Second):
		return fail(errors.New("timed out waiting for WhatsApp to start pairing"))
	}
	if phone == "" {
		return "", nil
	}
	code, err := wa.Client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		wa.mu.Lock()
		wa.pairErr = fmt.Errorf("failed to request pairing code: %w", err)
		wa.mu.Unlock()
		return "", wa.pairErr
	}
	wa.mu.Lock()
	wa.pairCode = code
	wa.mu.Unlock()
	return code, nil
}

func (wa *WhatsApp) consumeQR(qrChan <-chan whatsmeow.QRChannelItem, ready chan struct{}, onQR func(string)) {
	first := true
	for item := range qrChan {
		wa.mu.Lock()
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			wa.qrCode = item.Code
			_ = qrcode.WriteFile(item.Code, qrcode.Medium, 512, wa.qrPath)
		case whatsmeow.QRChannelSuccess.Event:
			wa.qrCode, wa.pairCode = "", ""
			_ = os.Remove(wa.qrPath)
			wa.log.Infof("Pairing successful")
		default:
			wa.qrCode, wa.pairCode = "", ""
			_ = os.Remove(wa.qrPath)
			if item.Error != nil {
				wa.pairErr = item.Error
			} else {
				wa.pairErr = fmt.Errorf("pairing ended: %s", item.Event)
			}
			wa.log.Warnf("Pairing ended: %v", wa.pairErr)
		}
		wa.mu.Unlock()
		if item.Event == whatsmeow.QRChannelEventCode && onQR != nil {
			onQR(item.Code)
		}
		if first {
			first = false
			close(ready)
		}
	}
	wa.mu.Lock()
	wa.pairing = false
	wa.mu.Unlock()
}

func (wa *WhatsApp) waitPairingDone(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		wa.mu.Lock()
		done := !wa.pairing
		wa.mu.Unlock()
		if done {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// WaitForLogin blocks until pairing finishes, returning nil once the device is paired.
func (wa *WhatsApp) WaitForLogin(ctx context.Context) error {
	for {
		_, _, _, pairing, err := wa.PairingState()
		if wa.LoggedIn() {
			return nil
		}
		if !pairing {
			if err == nil {
				err = errors.New("pairing ended")
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// PairingState returns the current pairing code and QR code (if pairing is in progress) and the last pairing error.
func (wa *WhatsApp) PairingState() (pairCode, qr, qrPath string, pairing bool, err error) {
	wa.mu.Lock()
	defer wa.mu.Unlock()
	return wa.pairCode, wa.qrCode, wa.qrPath, wa.pairing, wa.pairErr
}

func (wa *WhatsApp) SyncCount() int {
	wa.mu.Lock()
	defer wa.mu.Unlock()
	return wa.syncCount
}

func (wa *WhatsApp) Close() {
	wa.Client.Disconnect()
	_ = wa.Store.Close()
}

func (wa *WhatsApp) handleEvent(rawEvt any) {
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.Message:
		wa.storeMessage(ctx, evt)
	case *events.HistorySync:
		wa.storeHistorySync(ctx, evt.Data)
	case *events.Connected:
		wa.log.Infof("Connected to WhatsApp")
		go wa.refreshGroups(ctx)
	case *events.PushName:
		// Contact names are kept in whatsmeow's own store; nothing to do here.
	case *events.GroupInfo:
		if evt.Name != nil {
			_ = wa.Store.UpsertChat(ctx, evt.JID.String(), evt.Name.Name, true, time.Time{})
		}
	case *events.JoinedGroup:
		_ = wa.Store.UpsertChat(ctx, evt.JID.String(), evt.GroupName.Name, true, time.Time{})
	case *events.LoggedOut:
		wa.log.Warnf("Logged out from WhatsApp (reason: %s); run the login command again to re-pair", evt.Reason)
	}
}

// refreshGroups fetches the names of all joined groups, since messages don't carry them.
func (wa *WhatsApp) refreshGroups(ctx context.Context) {
	groups, err := wa.Client.GetJoinedGroups(ctx)
	if err != nil {
		wa.log.Warnf("Failed to fetch joined groups: %v", err)
		return
	}
	for _, g := range groups {
		_ = wa.Store.UpsertChat(ctx, g.JID.String(), g.Name, true, time.Time{})
	}
}

// normalizeJID maps LID-addressed users to their phone-number JID when the mapping is
// known, so the same person doesn't show up as two different chats.
func (wa *WhatsApp) normalizeJID(ctx context.Context, jid types.JID) types.JID {
	if jid.Server == types.HiddenUserServer {
		if pn, err := wa.Client.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return jid.ToNonAD()
}

// ContactName returns the best known display name for a user JID.
func (wa *WhatsApp) ContactName(ctx context.Context, jid types.JID) string {
	for _, candidate := range []types.JID{jid.ToNonAD(), wa.normalizeJID(ctx, jid)} {
		info, err := wa.Client.Store.Contacts.GetContact(ctx, candidate)
		if err != nil || !info.Found {
			continue
		}
		for _, name := range []string{info.FullName, info.FirstName, info.PushName, info.BusinessName} {
			if name != "" {
				return name
			}
		}
	}
	return ""
}

func (wa *WhatsApp) chatName(ctx context.Context, chat types.JID) string {
	if chat.Server == types.GroupServer {
		if existing, _ := wa.Store.GetChat(ctx, chat.String()); existing != nil && existing.Name != "" {
			return existing.Name
		}
		if info, err := wa.Client.GetGroupInfo(ctx, chat); err == nil {
			return info.Name
		}
		return ""
	}
	if name := wa.ContactName(ctx, chat); name != "" {
		return name
	}
	if chat.Server == types.DefaultUserServer {
		return "+" + chat.User
	}
	return ""
}

func (wa *WhatsApp) storeMessage(ctx context.Context, evt *events.Message) {
	chat := wa.normalizeJID(ctx, evt.Info.Chat)
	if chat.Server == types.BroadcastServer && chat.User == "status" {
		return // skip status updates
	}
	sender := wa.normalizeJID(ctx, evt.Info.Sender)
	if !evt.Info.SenderAlt.IsEmpty() && sender.Server == types.HiddenUserServer {
		sender = evt.Info.SenderAlt.ToNonAD()
	}

	// Edits and deletes arrive as protocol messages that point at an earlier message.
	if pm := evt.Message.GetProtocolMessage(); pm != nil {
		target := pm.GetKey().GetID()
		switch pm.GetType() {
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if text, _ := extractContent(pm.GetEditedMessage()); text != "" {
				_ = wa.Store.EditMessage(ctx, chat.String(), target, text)
			}
		case waE2E.ProtocolMessage_REVOKE:
			_ = wa.Store.DeleteMessage(ctx, chat.String(), target)
		}
		return
	}

	m := buildMessage(evt.Message)
	if m == nil {
		return
	}
	m.ChatJID = chat.String()
	m.ID = evt.Info.ID
	m.SenderJID = sender.String()
	m.FromMe = evt.Info.IsFromMe
	m.Timestamp = evt.Info.Timestamp
	m.SenderName = evt.Info.PushName
	if m.FromMe {
		m.SenderName = "me"
	} else if name := wa.ContactName(ctx, sender); name != "" {
		m.SenderName = name
	}

	if err := wa.Store.UpsertChat(ctx, m.ChatJID, wa.chatName(ctx, chat), chat.Server == types.GroupServer, m.Timestamp); err != nil {
		wa.log.Warnf("Failed to store chat %s: %v", m.ChatJID, err)
	}
	if err := wa.Store.SaveMessage(ctx, m); err != nil {
		wa.log.Warnf("Failed to store message %s: %v", m.ID, err)
	}
}

func (wa *WhatsApp) storeHistorySync(ctx context.Context, data *waHistorySync.HistorySync) {
	wa.mu.Lock()
	wa.syncCount++
	wa.mu.Unlock()
	stored := 0
	for _, conv := range data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		var last time.Time
		if ts := conv.GetConversationTimestamp(); ts > 0 {
			last = time.Unix(int64(ts), 0)
		}
		normalized := wa.normalizeJID(ctx, chatJID)
		if name == "" {
			name = wa.chatName(ctx, normalized)
		}
		_ = wa.Store.UpsertChat(ctx, normalized.String(), name, normalized.Server == types.GroupServer, last)
		for _, hm := range conv.GetMessages() {
			evt, err := wa.Client.ParseWebMessage(chatJID, hm.GetMessage())
			if err != nil {
				continue
			}
			wa.storeMessage(ctx, evt)
			stored++
		}
	}
	wa.log.Infof("History sync (%s): stored %d messages from %d conversations",
		data.GetSyncType(), stored, len(data.GetConversations()))
}

// buildMessage turns a WhatsApp message into a storable Message, or nil for
// message kinds that aren't worth keeping (reactions, receipts, key distribution, ...).
func buildMessage(msg *waE2E.Message) *Message {
	if msg == nil {
		return nil
	}
	text, kind := extractContent(msg)
	if kind == "" {
		return nil
	}
	m := &Message{Type: kind, Text: text}
	if ctxInfo := contextInfo(msg); ctxInfo != nil {
		m.QuotedID = ctxInfo.GetStanzaID()
	}
	switch {
	case msg.ImageMessage != nil:
		m.MediaType, m.MimeType = "image", msg.ImageMessage.GetMimetype()
		m.Raw, _ = proto.Marshal(&waE2E.Message{ImageMessage: msg.ImageMessage})
	case msg.VideoMessage != nil:
		m.MediaType, m.MimeType = "video", msg.VideoMessage.GetMimetype()
		m.Raw, _ = proto.Marshal(&waE2E.Message{VideoMessage: msg.VideoMessage})
	case msg.AudioMessage != nil:
		m.MediaType, m.MimeType = "audio", msg.AudioMessage.GetMimetype()
		m.Raw, _ = proto.Marshal(&waE2E.Message{AudioMessage: msg.AudioMessage})
	case msg.DocumentMessage != nil:
		m.MediaType, m.MimeType = "document", msg.DocumentMessage.GetMimetype()
		m.FileName = msg.DocumentMessage.GetFileName()
		m.Raw, _ = proto.Marshal(&waE2E.Message{DocumentMessage: msg.DocumentMessage})
	case msg.StickerMessage != nil:
		m.MediaType, m.MimeType = "sticker", msg.StickerMessage.GetMimetype()
		m.Raw, _ = proto.Marshal(&waE2E.Message{StickerMessage: msg.StickerMessage})
	}
	return m
}

// extractContent returns the human readable text of a message and its kind.
// An empty kind means the message has no user-visible content.
func extractContent(msg *waE2E.Message) (text, kind string) {
	switch {
	case msg == nil:
		return "", ""
	case msg.Conversation != nil:
		return msg.GetConversation(), "text"
	case msg.ExtendedTextMessage != nil:
		return msg.ExtendedTextMessage.GetText(), "text"
	case msg.ImageMessage != nil:
		return msg.ImageMessage.GetCaption(), "image"
	case msg.VideoMessage != nil:
		if msg.VideoMessage.GetGifPlayback() {
			return msg.VideoMessage.GetCaption(), "gif"
		}
		return msg.VideoMessage.GetCaption(), "video"
	case msg.AudioMessage != nil:
		if msg.AudioMessage.GetPTT() {
			return "", "voice_note"
		}
		return "", "audio"
	case msg.DocumentMessage != nil:
		return msg.DocumentMessage.GetCaption(), "document"
	case msg.StickerMessage != nil:
		return "", "sticker"
	case msg.LocationMessage != nil:
		l := msg.LocationMessage
		return strings.TrimSpace(fmt.Sprintf("%s %s (%f, %f)", l.GetName(), l.GetAddress(), l.GetDegreesLatitude(), l.GetDegreesLongitude())), "location"
	case msg.LiveLocationMessage != nil:
		l := msg.LiveLocationMessage
		return fmt.Sprintf("live location (%f, %f)", l.GetDegreesLatitude(), l.GetDegreesLongitude()), "location"
	case msg.ContactMessage != nil:
		return msg.ContactMessage.GetDisplayName(), "contact"
	case msg.ContactsArrayMessage != nil:
		var names []string
		for _, c := range msg.ContactsArrayMessage.GetContacts() {
			names = append(names, c.GetDisplayName())
		}
		return strings.Join(names, ", "), "contact"
	case msg.PollCreationMessage != nil, msg.PollCreationMessageV2 != nil, msg.PollCreationMessageV3 != nil:
		poll := msg.GetPollCreationMessage()
		if poll == nil {
			poll = msg.GetPollCreationMessageV2()
		}
		if poll == nil {
			poll = msg.GetPollCreationMessageV3()
		}
		var opts []string
		for _, o := range poll.GetOptions() {
			opts = append(opts, o.GetOptionName())
		}
		return poll.GetName() + " [" + strings.Join(opts, " / ") + "]", "poll"
	case msg.EventMessage != nil:
		return msg.EventMessage.GetName() + " " + msg.EventMessage.GetDescription(), "event"
	}
	return "", ""
}

func contextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	switch {
	case msg.ExtendedTextMessage != nil:
		return msg.ExtendedTextMessage.GetContextInfo()
	case msg.ImageMessage != nil:
		return msg.ImageMessage.GetContextInfo()
	case msg.VideoMessage != nil:
		return msg.VideoMessage.GetContextInfo()
	case msg.AudioMessage != nil:
		return msg.AudioMessage.GetContextInfo()
	case msg.DocumentMessage != nil:
		return msg.DocumentMessage.GetContextInfo()
	case msg.StickerMessage != nil:
		return msg.StickerMessage.GetContextInfo()
	}
	return nil
}

// RequestHistory asks the phone for older messages in a chat. The messages arrive
// asynchronously as a history sync and are stored automatically.
func (wa *WhatsApp) RequestHistory(ctx context.Context, chat types.JID, count int) error {
	oldest, err := wa.Store.OldestMessage(ctx, chat.String())
	if err != nil {
		return err
	}
	if oldest == nil {
		return errors.New("no messages stored for this chat yet, so there's no anchor to request older history from")
	}
	sender, _ := types.ParseJID(oldest.SenderJID)
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: oldest.FromMe, IsGroup: chat.Server == types.GroupServer},
		ID:            oldest.ID,
		Timestamp:     oldest.Timestamp,
	}
	_, err = wa.Client.SendPeerMessage(ctx, wa.Client.BuildHistorySyncRequest(info, count))
	return err
}

// recordSent stores a message we just sent so it shows up immediately in reads.
func (wa *WhatsApp) recordSent(ctx context.Context, to types.JID, id types.MessageID, ts time.Time, msg *waE2E.Message) {
	m := buildMessage(msg)
	if m == nil {
		return
	}
	m.ChatJID = to.String()
	m.ID = id
	m.FromMe = true
	m.SenderName = "me"
	if own := wa.Client.Store.ID; own != nil {
		m.SenderJID = own.ToNonAD().String()
	}
	m.Timestamp = ts
	_ = wa.Store.UpsertChat(ctx, m.ChatJID, wa.chatName(ctx, to), to.Server == types.GroupServer, ts)
	_ = wa.Store.SaveMessage(ctx, m)
}
