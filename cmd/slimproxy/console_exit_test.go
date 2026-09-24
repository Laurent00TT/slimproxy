package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDefaultLaunchPreservesBusyPortError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cx, _, stderr := newTestContext()
	path := writeTestConfig(t)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(body, []byte("port: 8317"), []byte("port: "+port), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(path))
	waits := 0
	code := runCLI(cx, nil, func() {
		waits++
		if !strings.Contains(stderr.String(), port) {
			t.Fatal("error must name the busy port before waiting for acknowledgement")
		}
	})
	if code != 1 || waits != 1 {
		t.Fatalf("exit=%d waits=%d, want 1 and 1", code, waits)
	}
}

func TestSuccessfulCLIExitsWithoutWaiting(t *testing.T) {
	cx, _, _ := newTestContext()
	if code := runCLI(cx, []string{"help"}, func() { t.Fatal("successful command waited") }); code != 0 {
		t.Fatalf("help exit=%d", code)
	}
}

func TestConsoleErrorWaitIsInteractiveOnly(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		reader := &acknowledgementReader{t: t, allowed: interactive}
		var output bytes.Buffer
		waitForConsoleError(interactive, reader, &output)
		if reader.read != interactive || (output.Len() > 0) != interactive {
			t.Fatalf("interactive=%v read=%v output=%q", interactive, reader.read, output.String())
		}
	}
}

type acknowledgementReader struct {
	t       *testing.T
	allowed bool
	read    bool
}

func (r *acknowledgementReader) Read(p []byte) (int, error) {
	if !r.allowed {
		r.t.Fatal("non-interactive invocation tried to read stdin")
	}
	r.read = true
	return copy(p, "\n"), io.EOF
}
