package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultLogFilePath(t *testing.T) {
	t.Parallel()

	if got, want := defaultLogFilePath(), filepath.Join(".vault", "wocproxy.log"); got != want {
		t.Fatalf("defaultLogFilePath()=%q, want %q", got, want)
	}
}

func TestBuildLoggerCreatesVaultLogFile(t *testing.T) {
	workdir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error: %v", err)
	}
	if err := os.Chdir(workdir); err != nil {
		t.Fatalf("Chdir() error: %v", err)
	}
	defer func() {
		_ = os.Chdir(oldwd)
	}()

	logger, closeLog, err := buildLogger("")
	if err != nil {
		t.Fatalf("buildLogger() error: %v", err)
	}
	defer func() {
		if closeLog != nil {
			_ = closeLog()
		}
	}()

	logger.Info("hello", "scope", "test")

	raw, err := os.ReadFile(filepath.Join(workdir, ".vault", "wocproxy.log"))
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if !strings.Contains(string(raw), "hello") {
		t.Fatalf("expected log file to contain message, got: %s", string(raw))
	}
}
