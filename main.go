// Command whatsapp-mcp is an MCP server that links to a WhatsApp account as a
// companion device (like WhatsApp Web) and lets an MCP client read and send messages.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"github.com/rs/zerolog"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const version = "0.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `whatsapp-mcp %s: a WhatsApp MCP server built on whatsmeow

Usage:
  whatsapp-mcp login [--phone +14155551234] [--qr]   Link your WhatsApp account (pairing code by default)
  whatsapp-mcp serve [--http :8080]                  Run the MCP server (stdio by default)
  whatsapp-mcp logout                                Unlink this device and delete the session

Global flags (before or after the command):
  --data DIR      Data directory (default $WHATSAPP_MCP_DATA or ~/.whatsapp-mcp)
  --log-level L   DEBUG, INFO, WARN or ERROR (default INFO; logs go to stderr)
`, version)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	dataDir := fs.String("data", defaultDataDir(), "data directory")
	logLevel := fs.String("log-level", "INFO", "log level")
	phone := fs.String("phone", os.Getenv("WHATSAPP_PHONE"), "phone number to pair with (login)")
	useQR := fs.Bool("qr", false, "pair by scanning a QR code instead of entering a code (login)")
	httpAddr := fs.String("http", "", "serve MCP over streamable HTTP on this address instead of stdio (serve)")
	_ = fs.Parse(args)

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
		err = runServe(ctx, *dataDir, *httpAddr, log)
	case "login":
		err = runLogin(ctx, *dataDir, *phone, *useQR, log)
	case "logout":
		err = runLogout(ctx, *dataDir, log)
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

func runServe(ctx context.Context, dataDir, httpAddr string, log waLog.Logger) error {
	wa, err := NewWhatsApp(ctx, dataDir, log)
	if err != nil {
		return err
	}
	defer wa.Close()
	if wa.LoggedIn() {
		if err = wa.Connect(); err != nil {
			log.Errorf("Failed to connect to WhatsApp: %v", err)
		}
	} else {
		log.Warnf("WhatsApp isn't linked yet. Run `whatsapp-mcp login`, or use the pair_device tool.")
	}

	s := NewMCPServer(wa, version)
	errc := make(chan error, 1)
	if httpAddr != "" {
		httpServer := server.NewStreamableHTTPServer(s)
		log.Infof("Serving MCP over HTTP at http://%s/mcp", httpAddr)
		go func() { errc <- httpServer.Start(httpAddr) }()
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return httpServer.Shutdown(shutdownCtx)
		case err = <-errc:
			return err
		}
	}
	go func() { errc <- server.ServeStdio(s) }()
	select {
	case <-ctx.Done():
		return nil
	case err = <-errc:
		return err
	}
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
