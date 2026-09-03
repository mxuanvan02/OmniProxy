package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"omniproxy/cli"
	"omniproxy/config"
	"omniproxy/logger"
	"omniproxy/pool"
	"omniproxy/proxy"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var pidPath string

func main() {
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create data directory: %v\n", err)
		os.Exit(1)
	}

	if err := config.Init(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	pidPath = cli.PIDPath(configPath)
	defer cli.RemovePID(pidPath)
	if cli.CheckAndKillExisting(pidPath) {
		os.Exit(0)
	}

	daemonMode := false
	useMenu := true
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--daemon":
			daemonMode = true
		case "--no-menu":
			useMenu = false
		}
	}

	if daemonMode {
		cli.RunDaemon(configPath)
		return
	}

	logger.Init(config.GetLogLevel())

	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		if err := config.SetPassword(envPassword); err != nil {
			fmt.Fprintf(os.Stderr, "ADMIN_PASSWORD rejected: %v\n", err)
			os.Exit(1)
		}
	}

	// Printed to stderr, once, and never logged: the ring buffer behind
	// /admin/api/logs is readable by any authenticated caller.
	if generated := config.TakeGeneratedPassword(); generated != "" {
		fmt.Fprintf(os.Stderr, "\n  Generated admin password: %s\n  Save it now — it is not shown again.\n\n", generated)
	}

	pool.GetPool()
	handler := proxy.NewHandler()

	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())

	if useMenu && cli.IsTerminal() {
		// In menu mode, suppress all log output to the terminal.
		// Logs are still captured in the ring buffer for the verbose log viewer.
		logger.SetOutput(io.Discard)
	} else {
		logger.Infof("OmniProxy starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
		logger.Infof("Admin panel: http://%s/admin", addr)
		logger.Infof("Claude API: http://%s/v1/messages", addr)
		logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)
	}

	srv := startServer(addr, handler)
	cli.WritePID(pidPath)

	if useMenu && cli.IsTerminal() {
		cli.ShowMenu(addr, pidPath, func() {
			shutdownServer(srv, handler)
		})
	} else {
		waitForSignal(func() {
			shutdownServer(srv, handler)
		})
	}
}

func startServer(addr string, handler *proxy.Handler) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		// Claude Code can upload a large context and tool state before the
		// handler starts streaming. Keep request-body timeout aligned with the
		// upstream client timeout so slow uploads are not cut at one minute.
		ReadTimeout: 15 * time.Minute,
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			logger.Fatalf("Server failed: %v", err)
		}
	}()
	return srv
}

func shutdownServer(srv *http.Server, handler *proxy.Handler) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	// Drain in-memory state after in-flight requests finish: background loops
	// only persist on a 30s tick, so the final window would otherwise be lost.
	handler.Shutdown()
	if err := config.FlushPendingSave(); err != nil {
		logger.Errorf("flush config on shutdown: %v", err)
	}
}

func waitForSignal(shutdown func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	shutdown()
}
