package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// Every account is identified by its WhatsApp phone number (digits only) and lives in
// its own directory, DATA/accounts/<number>/, with its own WhatsApp session, message
// database and files folder. Nothing is shared between accounts except the OAuth
// database, where every code and token is bound to exactly one account.

// NormalizePhone turns "+1 (415) 555-1234" into "14155551234".
func NormalizePhone(s string) (string, error) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		if strings.ContainsRune(" +-().", r) {
			return -1
		}
		return 'x'
	}, s)
	if strings.Contains(digits, "x") || len(digits) < 7 || len(digits) > 15 || digits[0] == '0' {
		return "", fmt.Errorf("%q isn't a phone number in international format (e.g. +14155551234)", s)
	}
	return digits, nil
}

func AccountDir(dataDir, id string) string {
	return filepath.Join(dataDir, "accounts", id)
}

// AccountStore holds accounts and OAuth state in DATA/auth.db.
type AccountStore struct {
	db *sql.DB
}

func OpenAccountStore(dataDir string) (*AccountStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "auth.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(authSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	restrictPermissions(path)
	return &AccountStore{db: db}, nil
}

func (s *AccountStore) Close() error { return s.db.Close() }

var ErrNoAccount = errors.New("no such account")

func (s *AccountStore) AddAccount(ctx context.Context, id, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO accounts (id, password_hash, created_at) VALUES (?, ?, ?)`,
		id, string(hash), time.Now().Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("an account for +%s already exists", id)
	}
	return err
}

// SetPassword changes an account's password and signs out all of its clients.
func (s *AccountStore) SetPassword(ctx context.Context, id, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE accounts SET password_hash = ? WHERE id = ?`, string(hash), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoAccount
	}
	_, err = s.RevokeTokens(ctx, id)
	return err
}

// RemoveAccount deletes an account; its codes and tokens go with it.
func (s *AccountStore) RemoveAccount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoAccount
	}
	// Foreign keys cascade, but be explicit in case the pragma was off when they were written.
	_, _ = s.RevokeTokens(ctx, id)
	return nil
}

func (s *AccountStore) PasswordHash(ctx context.Context, id string) ([]byte, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM accounts WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoAccount
	}
	return []byte(hash), err
}

type AccountInfo struct {
	ID        string
	CreatedAt time.Time
	Clients   int // live refresh tokens, i.e. signed-in clients
}

func (s *AccountStore) ListAccounts(ctx context.Context) ([]AccountInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.created_at,
		       (SELECT COUNT(*) FROM oauth_tokens t WHERE t.account_id = a.id AND t.kind = 'refresh' AND t.expires_at > ?)
		FROM accounts a ORDER BY a.created_at`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountInfo
	for rows.Next() {
		var a AccountInfo
		var created int64
		if err = rows.Scan(&a.ID, &created, &a.Clients); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// RevokeTokens signs out every client of one account, or of all accounts when id is empty.
func (s *AccountStore) RevokeTokens(ctx context.Context, id string) (int64, error) {
	var res sql.Result
	var err error
	if id == "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM oauth_tokens`)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM oauth_codes`)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE account_id = ?`, id)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM oauth_codes WHERE account_id = ?`, id)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AccountManager runs one WhatsApp client per account, starting each on first use.
type AccountManager struct {
	dataDir string
	store   *AccountStore
	log     waLog.Logger

	mu      sync.Mutex
	running map[string]*accountRuntime
}

type accountRuntime struct {
	wa    *WhatsApp
	tools *tools
}

func NewAccountManager(dataDir string, store *AccountStore, log waLog.Logger) *AccountManager {
	return &AccountManager{dataDir: dataDir, store: store, log: log, running: make(map[string]*accountRuntime)}
}

// Tools returns the tool implementation bound to one account, starting its WhatsApp
// client if needed.
func (m *AccountManager) Tools(ctx context.Context, id string) (*tools, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt := m.running[id]; rt != nil {
		return rt.tools, nil
	}
	if _, err := m.store.PasswordHash(ctx, id); err != nil {
		return nil, err
	}
	wa, err := StartAccount(context.Background(), m.dataDir, id, m.log.Sub("+"+id))
	if err != nil {
		return nil, err
	}
	rt := &accountRuntime{wa: wa, tools: &tools{wa: wa, filesDir: wa.FilesDir}}
	m.running[id] = rt
	return rt.tools, nil
}

// StartLinked starts every account that already has a WhatsApp session, so their
// messages keep syncing even before anyone calls a tool.
func (m *AccountManager) StartLinked(ctx context.Context) {
	accounts, err := m.store.ListAccounts(ctx)
	if err != nil {
		m.log.Errorf("Failed to list accounts: %v", err)
		return
	}
	for _, a := range accounts {
		if !IsLinked(ctx, AccountDir(m.dataDir, a.ID)) {
			continue
		}
		if _, err := m.Tools(ctx, a.ID); err != nil {
			m.log.Errorf("Failed to start account +%s: %v", a.ID, err)
		}
	}
}

// Reconcile stops clients of accounts that were removed while the server was running,
// unlinking them from WhatsApp so the phone no longer shows the device.
func (m *AccountManager) Reconcile(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		for id, rt := range m.running {
			if _, err := m.store.PasswordHash(ctx, id); errors.Is(err, ErrNoAccount) {
				m.log.Infof("Account +%s was removed; unlinking and stopping it", id)
				if rt.wa.LoggedIn() {
					_ = rt.wa.Client.Logout(ctx)
				}
				rt.wa.Close()
				delete(m.running, id)
			}
		}
		m.mu.Unlock()
	}
}

func (m *AccountManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rt := range m.running {
		rt.wa.Close()
		delete(m.running, id)
	}
}

// IsLinked reports whether an account directory holds a paired WhatsApp device,
// without opening (or creating) the session the way NewWhatsApp would.
func IsLinked(ctx context.Context, dir string) bool {
	path := filepath.Join(dir, "whatsapp.db")
	if _, err := os.Stat(path); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", sqliteDSN(path)+"&mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	return db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whatsmeow_device`).Scan(&n) == nil && n > 0
}

// StartAccount opens an account's WhatsApp session and connects it if it's linked.
func StartAccount(ctx context.Context, dataDir, id string, log waLog.Logger) (*WhatsApp, error) {
	wa, err := NewWhatsApp(ctx, AccountDir(dataDir, id), id, log)
	if err != nil {
		return nil, err
	}
	if wa.LoggedIn() {
		if err = wa.Connect(); err != nil {
			log.Errorf("Failed to connect to WhatsApp: %v", err)
		}
	}
	return wa, nil
}
