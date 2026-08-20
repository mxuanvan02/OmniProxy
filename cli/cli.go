// Package cli provides the interactive terminal interface for OmniProxy.
package cli

import (
	"context"
	"fmt"
	"net/http"
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

// ShowMenu displays the interactive management menu.
// addr is the server address; shutdown is called on Exit.
func ShowMenu(addr string, pidPath string, shutdown func()) {
	showMenu(addr, pidPath, shutdown)
}

// RunDaemon starts OmniProxy in background daemon mode, logging to a file,
// and blocks until a termination signal is received.
func RunDaemon(configPath string) {
	logFile := filepath.Join(filepath.Dir(configPath), "omniproxy.log")
	f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	os.Stdout = f
	os.Stderr = f

	pidPath := PIDPath(configPath)
	defer RemovePID(pidPath)
	if CheckAndKillExisting(pidPath) {
		os.Exit(0)
	}

	logger.Init(config.GetLogLevel())
	logger.SetOutput(f)

	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	pool.GetPool()
	handler := proxy.NewHandler()

	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		// Claude Code can upload a large context and tool state before the
		// handler starts streaming. Keep daemon mode aligned with main.go.
		ReadTimeout: 15 * time.Minute,
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			logger.Fatalf("Server failed: %v", err)
		}
	}()

	WritePID(pidPath)
	logger.Infof("OmniProxy started in background on http://%s (PID: %d)", addr, os.Getpid())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// IsTerminal reports whether stdout is a character device (terminal).
func IsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
