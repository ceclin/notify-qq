package app_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testProcessHealthcheck(t *testing.T, binary string) {
	directory := t.TempDir()
	dataDir := filepath.Join(directory, "uncreated-data")
	var status atomic.Int32
	status.Store(http.StatusOK)
	var delayed, redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/notify/readyz" {
			redirected.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		if delayed.Load() {
			<-r.Context().Done()
			return
		}
		if status.Load() == http.StatusFound {
			w.Header().Set("Location", "/redirect-target")
		}
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, "private-probe-response")
	}))
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	probe := func(success bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "healthcheck")
		command.Dir = directory
		command.Env = append(os.Environ(), "LISTEN_ADDR=0.0.0.0:"+port, "API_PATH_PREFIX=/api/notify/", "QQ_APP_ID=", "QQ_APP_SECRET=", "DATA_DIR="+dataDir, "LOG_LEVEL=invalid", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=", "GORACE=atexit_sleep_ms=0")
		output, err := command.CombinedOutput()
		if success {
			if err != nil || len(output) != 0 {
				t.Fatalf("healthy probe failed or emitted output: %v\n%s", err, output)
			}
		} else {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "healthcheck_failed") {
				t.Fatalf("unhealthy probe did not exit with its diagnostic: %v\n%s", err, output)
			}
		}
		if strings.Contains(string(output), "private-probe-response") {
			t.Fatal("probe printed the HTTP response body")
		}
		if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("probe opened application storage")
		}
	}
	probe(true)
	status.Store(http.StatusServiceUnavailable)
	probe(false)
	status.Store(http.StatusFound)
	probe(false)
	if redirected.Load() {
		t.Fatal("probe followed a redirect")
	}
	delayed.Store(true)
	probe(false)
	server.Close()
	probe(false)
}
