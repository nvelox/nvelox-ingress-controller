// Package reloader signals the nvelox sidecar to re-read its config.
//
// nvelox supports hot reload via SIGHUP (see nvelox/README.md: "Hot
// reload: kill -HUP $(pidof nvelox)"). We rely on the pod running with
// shareProcessNamespace: true so the controller container can see and
// signal the nvelox container's processes.
//
// PID discovery prefers the pid_file written by nvelox at boot (set
// in the shared ConfigMap to a path on the shared emptyDir). The
// /proc scan is a fallback for the brief window between pod start and
// nvelox writing its pid_file — without it the first reconcile would
// no-op and we'd wait for the next change to take effect.
//
// SIGHUP semantics in nvelox: pre-flight validates the new file, then
// either applies atomically or rolls back. So a malformed render here
// can't take the data plane down — at worst the reload is rejected
// and the previous config keeps serving. We log either way.
package reloader

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Reloader writes the rendered config to disk and signals nvelox when
// the contents actually changed. Concurrency-safe: callers may invoke
// Apply from multiple reconciles; an internal mutex serializes writes
// so two parallel reconciles don't race on the file.
type Reloader struct {
	ConfigPath string // e.g. /etc/nvelox/conf.d/k8s.yaml
	PIDFile    string // e.g. /var/run/nvelox/nvelox.pid (shared emptyDir)
	ProcName   string // e.g. "nvelox" — used by the /proc fallback

	mu       sync.Mutex
	lastHash string
}

// Apply writes `data` to ConfigPath atomically if its contents differ
// from the previous write, then signals nvelox. Returns (changed, err):
//   - changed == false, err == nil → no-op, contents matched last write
//   - changed == true,  err == nil → wrote + signaled successfully
//   - err != nil → write or signal failed; previous nvelox state intact
func (r *Reloader) Apply(data []byte) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:])
	if hash == r.lastHash {
		return false, nil
	}

	// Atomic write: render to a temp file in the same directory, then
	// rename. Same-fs rename is the only way to guarantee nvelox never
	// SIGHUPs on a half-written file.
	dir := filepath.Dir(r.ConfigPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".nvelox-ingress-*.yaml")
	if err != nil {
		return false, fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return false, fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, r.ConfigPath); err != nil {
		os.Remove(tmpName)
		return false, fmt.Errorf("rename to %s: %w", r.ConfigPath, err)
	}

	pid, err := r.findPID()
	if err != nil {
		// The write succeeded — nvelox will pick up the new file on
		// its next natural reload (SIGHUP from anywhere, or pod
		// restart). Don't roll the file back: the next successful
		// reload should use the latest desired state, not the prior.
		return true, fmt.Errorf("locate nvelox pid: %w", err)
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		return true, fmt.Errorf("signal nvelox (pid %d): %w", pid, err)
	}

	r.lastHash = hash
	slog.Info("nvelox reloaded", "pid", pid, "path", r.ConfigPath, "bytes", len(data))
	return true, nil
}

// findPID prefers the pid_file (deterministic, single-source-of-truth)
// and falls back to scanning /proc for a process whose comm matches
// ProcName. Both paths require shareProcessNamespace=true on the pod.
func (r *Reloader) findPID() (int, error) {
	if r.PIDFile != "" {
		if pid, err := readPIDFile(r.PIDFile); err == nil {
			if pidAlive(pid) {
				return pid, nil
			}
		}
	}
	if r.ProcName != "" {
		if pid, err := pidByComm(r.ProcName); err == nil {
			return pid, nil
		}
	}
	return 0, errors.New("nvelox process not found (pid_file missing and /proc scan empty)")
}

func readPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, fmt.Errorf("empty pid file: %s", path)
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse pid %q: %w", s, err)
	}
	if pid <= 1 {
		return 0, fmt.Errorf("implausible pid %d", pid)
	}
	return pid, nil
}

func pidAlive(pid int) bool {
	// Kill with signal 0 doesn't actually signal — just checks
	// whether the caller could signal the target (i.e., the PID
	// exists and we have permission). Standard liveness probe.
	return syscall.Kill(pid, 0) == nil
}

// pidByComm walks /proc/<pid>/comm. Skips kernel threads (no exe
// link), our own PID, and PID 1 (the pause container).
func pidByComm(name string) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}
	self := os.Getpid()
	want := []byte(name + "\n")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		if bytes.Equal(comm, want) {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no /proc match for comm=%q", name)
}

// WaitForNvelox blocks until findPID succeeds or timeout elapses.
// Useful at boot — the reconciler's first Apply may race the nvelox
// container's startup; this lets the controller hold the first
// reconcile until nvelox is up so the SIGHUP lands on something.
func (r *Reloader) WaitForNvelox(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := r.findPID(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nvelox did not start within %s", timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
