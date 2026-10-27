// Command whatsapp-mcp is an MCP server that links to a WhatsApp account as a
// companion device (like WhatsApp Web) and lets an MCP client read and send messages.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
	waLog "go.mau.fi/whatsmeow/util/log"
	"golang.org/x/term"
)

const version = "0.3.0"

func usage() {
	fmt.Fprintf(os.Stderr, `whatsapp-mcp %s: a WhatsApp MCP server built on whatsmeow

Each WhatsApp number is a separate, isolated account with its own password, session,
messages and files.

Remote connector (OAuth sign-in, many accounts):
  whatsapp-mcp user add +14155551234      Create an account and set its password
  whatsapp-mcp user passwd +14155551234   Change an account's password (signs out its clients)
  whatsapp-mcp user remove +14155551234 [--delete-data]
                                          Remove an account and sign out its clients
  whatsapp-mcp user list                  List accounts
  whatsapp-mcp revoke-tokens [--phone N]  Sign out the clients of one account, or all
  whatsapp-mcp serve --http :8080 --public-url https://wa.example.com

Local use (stdio, one account, no password):
  whatsapp-mcp login --phone +14155551234 [--qr]   Link WhatsApp (pairing code by default)
  whatsapp-mcp serve [--phone +14155551234]        Serve over stdio
  whatsapp-mcp logout [--phone +14155551234]       Unlink this device from WhatsApp

Flags (each can also be set with the environment variable shown):
  --data DIR             WHATSAPP_MCP_DATA        Data directory (default ~/.whatsapp-mcp)
  --phone NUMBER         WHATSAPP_PHONE           Account for login, logout and stdio serve
  --http ADDR            WHATSAPP_MCP_HTTP        Serve over HTTP on ADDR instead of stdio
  --public-url URL       WHATSAPP_MCP_PUBLIC_URL  The https:// URL clients use to reach the server
  --read-only            WHATSAPP_MCP_READ_ONLY   Leave out every tool that sends messages
  --allow-redirect LIST  WHATSAPP_MCP_ALLOW_REDIRECTS  Extra OAuth redirect URI prefixes, comma separated
  --allowed-hosts LIST   WHATSAPP_MCP_ALLOWED_HOSTS    Extra Host headers to accept, comma separated
  --log-level LEVEL      WHATSAPP_MCP_LOG_LEVEL   debug, info, warn or error (logs go to stderr)
`, version)
}

type config struct {
	dataDir, phone, httpAddr, publicURL string
	readOnly, deleteData, useQR         bool
	allowRedirects, allowedHosts        []string
}

func main() {
	var cfg config
	fs := flag.NewFlagSet("whatsapp-mcp", flag.ExitOnError)
	fs.Usage = usage
	fs.StringVar(&cfg.dataDir, "data", defaultDataDir(), "")
	fs.StringVar(&cfg.phone, "phone", os.Getenv("WHATSAPP_PHONE"), "")
	fs.StringVar(&cfg.httpAddr, "http", os.Getenv("WHATSAPP_MCP_HTTP"), "")
	fs.StringVar(&cfg.publicURL, "public-url", os.Getenv("WHATSAPP_MCP_PUBLIC_URL"), "")
	fs.BoolVar(&cfg.readOnly, "read-only", envBool("WHATSAPP_MCP_READ_ONLY"), "")
	fs.BoolVar(&cfg.deleteData, "delete-data", false, "")
	fs.BoolVar(&cfg.useQR, "qr", false, "")
	allowRedirects := fs.String("allow-redirect", os.Getenv("WHATSAPP_MCP_ALLOW_REDIRECTS"), "")
	allowedHosts := fs.String("allowed-hosts", os.Getenv("WHATSAPP_MCP_ALLOWED_HOSTS"), "")
	logLevel := fs.String("log-level", envOr("WHATSAPP_MCP_LOG_LEVEL", "info"), "")
	// Flags may appear anywhere: before the command, between its words, or after them.
	var positional []string
	rest := os.Args[1:]
	for {
		_ = fs.Parse(rest)
		if rest = fs.Args(); len(rest) == 0 {
			break
		}
		positional, rest = append(positional, rest[0]), rest[1:]
	}
	cmd := "serve"
	if len(positional) > 0 {
		cmd, positional = positional[0], positional[1:]
	}
	cfg.allowRedirects = splitList(*allowRedirects)
	cfg.allowedHosts = splitList(*allowedHosts)

	// stdout belongs to the MCP protocol in stdio mode, so all logging goes to stderr.
	level, err := zerolog.ParseLevel(strings.ToLower(*logLevel))
	if err != nil {
		level = zerolog.InfoLevel
	}
	log := waLog.Zerolog(zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.DateTime}).
		Level(level).With().Timestamp().Logger())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch cmd {
	case "serve":
		if cfg.httpAddr != "" {
			err = runServeHTTP(ctx, cfg, log)
		} else {
			err = runServeStdio(ctx, cfg, log)
		}
	case "login":
		err = runLogin(ctx, cfg, log)
	case "logout":
		err = runLogout(ctx, cfg, log)
	case "user":
		err = runUser(ctx, cfg, positional)
	case "revoke-tokens":
		err = runRevokeTokens(ctx, cfg)
	case "healthcheck":
		err = runHealthcheck(cfg.httpAddr)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	if dir := os.Getenv("WHATSAPP_MCP_DATA"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".whatsapp-mcp"
	}
	return filepath.Join(home, ".whatsapp-mcp")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	v, _ := strconv.ParseBool(os.Getenv(key))
	return v
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// localAccount picks the account for local commands: --phone if given, otherwise the
// only account in the data directory.
func localAccount(cfg config) (string, error) {
	if cfg.phone != "" {
		return NormalizePhone(cfg.phone)
	}
	entries, _ := os.ReadDir(filepath.Join(cfg.dataDir, "accounts"))
	var ids []string
	for _, e := range entries {
		if _, err := NormalizePhone(e.Name()); e.IsDir() && err == nil {
			ids = append(ids, e.Name())
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", errors.New("no account yet; pass --phone with your WhatsApp number")
	default:
		return "", fmt.Errorf("several accounts exist (+%s); choose one with --phone", strings.Join(ids, ", +"))
	}
}

func runServeStdio(ctx context.Context, cfg config, log waLog.Logger) error {
	id, err := localAccount(cfg)
	if err != nil {
		return err
	}
	wa, err := StartAccount(ctx, cfg.dataDir, id, log)
	if err != nil {
		return err
	}
	defer wa.Close()
	if !wa.LoggedIn() {
		log.Warnf("WhatsApp isn't linked yet. Use the pair_device tool, or run `whatsapp-mcp login`.")
	}
	s := NewMCPServer(SingleAccount(wa), version, cfg.readOnly)
	errc := make(chan error, 1)
	go func() { errc <- server.ServeStdio(s) }()
	select {
	case <-ctx.Done():
		return nil
	case err = <-errc:
		return err
	}
}

func runServeHTTP(ctx context.Context, cfg config, log waLog.Logger) error {
	if cfg.publicURL == "" {
		return errors.New("--public-url is required with --http (the https:// address clients will use)")
	}
	store, err := OpenAccountStore(cfg.dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	redirects := defaultRedirects
	redirects.Prefixes = cfg.allowRedirects
	auth, err := NewAuthServer(store, cfg.publicURL, redirects)
	if err != nil {
		return err
	}

	accounts := NewAccountManager(cfg.dataDir, store, log)
	defer accounts.Close()
	accounts.StartLinked(ctx)
	go accounts.Reconcile(ctx)
	if list, _ := store.ListAccounts(ctx); len(list) == 0 {
		log.Warnf("No accounts yet. Create one with: whatsapp-mcp user add +<your number>")
	}

	resolve := func(ctx context.Context) (*tools, error) {
		id, ok := AccountFromContext(ctx)
		if !ok {
			return nil, errors.New("not signed in")
		}
		return accounts.Tools(ctx, id)
	}
	s := NewMCPServer(resolve, version, cfg.readOnly)
	log.Infof("Connector URL: %s", auth.ResourceURL())
	log.Infof("Listening on %s", cfg.httpAddr)
	return ServeHTTP(ctx, cfg.httpAddr, NewHTTPHandler(s, auth, cfg.publicURL, cfg.allowedHosts))
}

// runHealthcheck lets container runtimes probe the server without needing curl in the image.
func runHealthcheck(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("healthcheck needs --http or WHATSAPP_MCP_HTTP")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}

func runUser(ctx context.Context, cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: whatsapp-mcp user add|passwd|remove|list [+number]")
	}
	store, err := OpenAccountStore(cfg.dataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	action := args[0]
	if action == "list" {
		list, err := store.ListAccounts(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("No accounts.")
		}
		for _, a := range list {
			linked := "WhatsApp not linked"
			if IsLinked(ctx, AccountDir(cfg.dataDir, a.ID)) {
				linked = "WhatsApp linked"
			}
			fmt.Printf("+%-16s created %s, %d signed-in clients, %s\n", a.ID, a.CreatedAt.Format("2006-01-02"), a.Clients, linked)
		}
		return nil
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: whatsapp-mcp user %s +number", action)
	}
	id, err := NormalizePhone(args[1])
	if err != nil {
		return err
	}
	switch action {
	case "add":
		password, generated, err := readNewPassword()
		if err != nil {
			return err
		}
		if err = store.AddAccount(ctx, id, password); err != nil {
			return err
		}
		if err = os.MkdirAll(AccountDir(cfg.dataDir, id), 0o700); err != nil {
			return err
		}
		if generated {
			fmt.Printf("Password for +%s: %s\n", id, password)
		}
		fmt.Printf("Account +%s created. Add the connector in Claude and sign in with this number and password,\n", id)
		fmt.Println("then ask Claude to link WhatsApp (pair_device). Only this number can be linked to the account.")
	case "passwd":
		password, generated, err := readNewPassword()
		if err != nil {
			return err
		}
		if err = store.SetPassword(ctx, id, password); err != nil {
			return err
		}
		if generated {
			fmt.Printf("Password for +%s: %s\n", id, password)
		}
		fmt.Println("Password changed; the account's clients were signed out.")
	case "remove":
		if err = store.RemoveAccount(ctx, id); err != nil {
			return err
		}
		fmt.Printf("Account +%s removed and its clients signed out. A running server unlinks its WhatsApp within a minute.\n", id)
		if cfg.deleteData {
			if err = os.RemoveAll(AccountDir(cfg.dataDir, id)); err != nil {
				return err
			}
			fmt.Println("Its messages, files and WhatsApp session were deleted.")
		} else {
			fmt.Printf("Its data is still in %s; pass --delete-data to delete it.\n", AccountDir(cfg.dataDir, id))
		}
	default:
		return fmt.Errorf("unknown user action %q", action)
	}
	return nil
}

// readNewPassword asks for a password on the terminal, or generates one if left empty.
func readNewPassword() (password string, generated bool, err error) {
	fmt.Printf("Password (at least %d characters; leave empty to generate one): ", minPasswordChars)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", false, err
		}
		password = string(raw)
	} else {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		password = strings.TrimSpace(line)
		fmt.Println()
	}
	if password == "" {
		return GeneratePassword(), true, nil
	}
	return password, false, nil
}

func runRevokeTokens(ctx context.Context, cfg config) error {
	store, err := OpenAccountStore(cfg.dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	id := ""
	if cfg.phone != "" {
		if id, err = NormalizePhone(cfg.phone); err != nil {
			return err
		}
	}
	n, err := store.RevokeTokens(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("Revoked %d tokens; those clients have to sign in again.\n", n)
	return nil
}

func runLogin(ctx context.Context, cfg config, log waLog.Logger) error {
	id, err := localAccount(cfg)
	if err != nil && cfg.phone == "" {
		fmt.Print("Your WhatsApp phone number in international format (e.g. +14155551234): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		cfg.phone = strings.TrimSpace(line)
		id, err = localAccount(cfg)
	}
	if err != nil {
		return err
	}
	wa, err := NewWhatsApp(ctx, AccountDir(cfg.dataDir, id), id, log)
	if err != nil {
		return err
	}
	defer wa.Close()
	if wa.LoggedIn() {
		fmt.Printf("+%s is already linked. Run `whatsapp-mcp logout --phone +%s` first to link it again.\n", id, id)
		return nil
	}

	phone := id
	if cfg.useQR {
		phone = ""
	}
	showQR := func(code string) {
		if !cfg.useQR {
			return
		}
		fmt.Println("\nScan this QR code in WhatsApp > Settings > Linked devices > Link a device:")
		qrterminal.GenerateHalfBlock(code, qrterminal.L, os.Stdout)
	}
	code, err := wa.StartPairing(ctx, phone, showQR)
	if err != nil {
		return err
	}
	if code != "" {
		fmt.Printf("\nPairing code: %s\n\n", code)
		fmt.Println("On your phone: WhatsApp > Settings > Linked devices > Link a device >")
		fmt.Println("\"Link with phone number instead\", then enter the code above.")
		fmt.Println("(WhatsApp may also show a notification you can tap.) The code expires in about 2.5 minutes.")
		fmt.Println("Run again with --qr to scan a QR code instead.")
	}

	if err = wa.WaitForLogin(ctx); err != nil {
		return fmt.Errorf("pairing failed: %w", err)
	}
	fmt.Printf("\nLinked to +%s. Syncing chat history from your phone; this can take a few minutes.\n", id)
	fmt.Println("Keep your phone online. Press Ctrl+C to stop early (the MCP server keeps syncing when it runs).")
	waitForHistorySync(ctx, wa)
	return nil
}

// waitForHistorySync shows sync progress until no new messages arrive for a while.
func waitForHistorySync(ctx context.Context, wa *WhatsApp) {
	const idleLimit = 90 * time.Second
	lastCount, lastChange := -1, time.Now()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return
		case <-ticker.C:
		}
		chats, msgs, err := wa.Store.Stats(ctx)
		if err != nil {
			continue
		}
		if msgs != lastCount {
			lastCount, lastChange = msgs, time.Now()
			fmt.Printf("\r  %d chats, %d messages stored   ", chats, msgs)
		}
		if time.Since(lastChange) > idleLimit {
			fmt.Println("\nHistory sync looks finished. You can now start the MCP server.")
			return
		}
	}
}

func runLogout(ctx context.Context, cfg config, log waLog.Logger) error {
	id, err := localAccount(cfg)
	if err != nil {
		return err
	}
	wa, err := NewWhatsApp(ctx, AccountDir(cfg.dataDir, id), id, log)
	if err != nil {
		return err
	}
	defer wa.Close()
	if !wa.LoggedIn() {
		fmt.Println("Not linked.")
		return nil
	}
	if err = wa.Connect(); err != nil {
		return err
	}
	if err = wa.Client.Logout(ctx); err != nil {
		return err
	}
	fmt.Printf("+%s logged out. Its stored messages are kept in %s.\n", id, AccountDir(cfg.dataDir, id))
	return nil
}
