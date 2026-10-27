// Command whatsapp-mcp is an MCP server that links to a WhatsApp account as a
// companion device (like WhatsApp Web) and lets an MCP client read and send messages.
package main

import (
	"bufio"
	"context"
	"database/sql"
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

const version = "0.2.0"

func usage() {
	fmt.Fprintf(os.Stderr, `whatsapp-mcp %s: a WhatsApp MCP server built on whatsmeow

Usage:
  whatsapp-mcp login [--phone +14155551234] [--qr]  Link your WhatsApp account (pairing code by default)
  whatsapp-mcp serve                                Run the MCP server over stdio (local clients)
  whatsapp-mcp serve --http :8080 --public-url https://wa.example.com
                                                    Run it as a remote connector with OAuth sign-in
  whatsapp-mcp set-password                         Set the password that approves new connections
  whatsapp-mcp revoke-tokens                        Sign out every connected client
  whatsapp-mcp logout                               Unlink this device from your WhatsApp account

Flags (each can also be set with the environment variable shown):
  --data DIR             WHATSAPP_MCP_DATA        Data directory (default ~/.whatsapp-mcp)
  --files-dir DIR        WHATSAPP_MCP_FILES_DIR   Folder for downloads and files to send (default DATA/files)
  --http ADDR            WHATSAPP_MCP_HTTP        Serve over HTTP on ADDR instead of stdio
  --public-url URL       WHATSAPP_MCP_PUBLIC_URL  The https:// URL clients use to reach the server
  --read-only            WHATSAPP_MCP_READ_ONLY   Leave out every tool that sends messages
  --allow-redirect LIST  WHATSAPP_MCP_ALLOW_REDIRECTS  Extra OAuth redirect URI prefixes, comma separated
  --allowed-hosts LIST   WHATSAPP_MCP_ALLOWED_HOSTS    Extra Host headers to accept, comma separated
  --no-auth                                       Skip OAuth (only allowed on a loopback address)
  --phone NUMBER         WHATSAPP_PHONE           Phone number for pairing codes
  --log-level LEVEL      WHATSAPP_MCP_LOG_LEVEL   debug, info, warn or error (logs go to stderr)

WHATSAPP_MCP_PASSWORD sets the connection password without storing it; otherwise one is
generated on first start and printed once.
`, version)
}

type config struct {
	dataDir, filesDir, httpAddr, publicURL string
	readOnly, noAuth                       bool
	allowRedirects, allowedHosts           []string
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var cfg config
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	fs.StringVar(&cfg.dataDir, "data", defaultDataDir(), "")
	fs.StringVar(&cfg.filesDir, "files-dir", os.Getenv("WHATSAPP_MCP_FILES_DIR"), "")
	fs.StringVar(&cfg.httpAddr, "http", os.Getenv("WHATSAPP_MCP_HTTP"), "")
	fs.StringVar(&cfg.publicURL, "public-url", os.Getenv("WHATSAPP_MCP_PUBLIC_URL"), "")
	fs.BoolVar(&cfg.readOnly, "read-only", envBool("WHATSAPP_MCP_READ_ONLY"), "")
	fs.BoolVar(&cfg.noAuth, "no-auth", false, "")
	allowRedirects := fs.String("allow-redirect", os.Getenv("WHATSAPP_MCP_ALLOW_REDIRECTS"), "")
	allowedHosts := fs.String("allowed-hosts", os.Getenv("WHATSAPP_MCP_ALLOWED_HOSTS"), "")
	phone := fs.String("phone", os.Getenv("WHATSAPP_PHONE"), "")
	useQR := fs.Bool("qr", false, "")
	logLevel := fs.String("log-level", envOr("WHATSAPP_MCP_LOG_LEVEL", "info"), "")
	_ = fs.Parse(args)
	cfg.allowRedirects = splitList(*allowRedirects)
	cfg.allowedHosts = splitList(*allowedHosts)
	if cfg.filesDir == "" {
		cfg.filesDir = filepath.Join(cfg.dataDir, "files")
	}

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
		err = runServe(ctx, cfg, log)
	case "login":
		err = runLogin(ctx, cfg.dataDir, *phone, *useQR, log)
	case "logout":
		err = runLogout(ctx, cfg.dataDir, log)
	case "set-password":
		err = runSetPassword(cfg.dataDir)
	case "revoke-tokens":
		err = runRevokeTokens(ctx, cfg.dataDir)
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

func runServe(ctx context.Context, cfg config, log waLog.Logger) error {
	if err := os.MkdirAll(cfg.filesDir, 0o700); err != nil {
		return err
	}
	if cfg.httpAddr != "" {
		// Validate the HTTP setup before touching WhatsApp so mistakes fail fast.
		if cfg.noAuth {
			host, _, err := net.SplitHostPort(cfg.httpAddr)
			if err != nil || !isLoopbackHost(host) {
				return errors.New("--no-auth is only allowed when listening on a loopback address such as 127.0.0.1:8765")
			}
		} else if cfg.publicURL == "" {
			return errors.New("--public-url is required with --http (the https:// address clients will use); " +
				"use --no-auth on 127.0.0.1 for local-only testing")
		}
	}

	wa, err := NewWhatsApp(ctx, cfg.dataDir, log)
	if err != nil {
		return err
	}
	defer wa.Close()
	if wa.LoggedIn() {
		if err = wa.Connect(); err != nil {
			log.Errorf("Failed to connect to WhatsApp: %v", err)
		}
	} else {
		log.Warnf("WhatsApp isn't linked yet. Use the pair_device tool, or run `whatsapp-mcp login`.")
	}

	s := NewMCPServer(wa, version, ToolOptions{ReadOnly: cfg.readOnly, FilesDir: cfg.filesDir})
	if cfg.httpAddr == "" {
		errc := make(chan error, 1)
		go func() { errc <- server.ServeStdio(s) }()
		select {
		case <-ctx.Done():
			return nil
		case err = <-errc:
			return err
		}
	}

	var auth *AuthServer
	if !cfg.noAuth {
		hash, generated, err := LoadPasswordHash(filepath.Join(cfg.dataDir, "password.hash"))
		if err != nil {
			return fmt.Errorf("password setup failed: %w", err)
		}
		if generated != "" {
			fmt.Fprintf(os.Stderr, "\n"+
				"  ============================================================\n"+
				"  Connection password (shown only once, save it somewhere safe):\n\n"+
				"      %s\n\n"+
				"  You'll type it when adding this server as a connector.\n"+
				"  Change it any time with: whatsapp-mcp set-password\n"+
				"  ============================================================\n\n", generated)
		}
		redirects := defaultRedirects
		redirects.Prefixes = cfg.allowRedirects
		auth, err = NewAuthServer(filepath.Join(cfg.dataDir, "auth.db"), cfg.publicURL, hash, redirects)
		if err != nil {
			return err
		}
		defer auth.Close()
		log.Infof("Connector URL: %s", auth.ResourceURL())
	} else {
		log.Warnf("OAuth is disabled; anything that can reach %s can use your WhatsApp", cfg.httpAddr)
	}
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

func runSetPassword(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	fmt.Printf("New connection password (at least %d characters; leave empty to generate one): ", minPasswordChars)
	var password string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		password = string(raw)
	} else {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		password = strings.TrimSpace(line)
	}
	if password == "" {
		password = GeneratePassword()
		fmt.Printf("Generated password: %s\n", password)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dataDir, "password.hash"), hash, 0o600); err != nil {
		return err
	}
	fmt.Println("Password saved. It applies the next time the server starts (WHATSAPP_MCP_PASSWORD, if set, takes precedence).")
	fmt.Println("Existing connections stay signed in; run `whatsapp-mcp revoke-tokens` to sign them out.")
	return nil
}

func runRevokeTokens(ctx context.Context, dataDir string) error {
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dataDir, "auth.db")))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, authSchema); err != nil {
		return err
	}
	a := &AuthServer{db: db}
	n, err := a.RevokeAll(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Revoked %d tokens. Every client has to sign in again with the password.\n", n)
	return nil
}

func runLogin(ctx context.Context, dataDir, phone string, useQR bool, log waLog.Logger) error {
	wa, err := NewWhatsApp(ctx, dataDir, log)
	if err != nil {
		return err
	}
	defer wa.Close()
	if wa.LoggedIn() {
		fmt.Printf("Already linked to +%s. Run `whatsapp-mcp logout` first to link a different account.\n", wa.Client.Store.ID.User)
		return nil
	}

	if !useQR && phone == "" {
		fmt.Print("Your WhatsApp phone number in international format (e.g. +14155551234),\nor leave empty to scan a QR code instead: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		phone = strings.TrimSpace(line)
		useQR = phone == ""
	}
	if useQR {
		phone = ""
	}

	showQR := func(code string) {
		if !useQR {
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
	fmt.Printf("\nLinked to +%s. Syncing chat history from your phone; this can take a few minutes.\n", wa.Client.Store.ID.User)
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

func runLogout(ctx context.Context, dataDir string, log waLog.Logger) error {
	wa, err := NewWhatsApp(ctx, dataDir, log)
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
	fmt.Println("Logged out. Stored messages are kept in", filepath.Join(dataDir, "messages.db"), "(delete it to remove them).")
	return nil
}
