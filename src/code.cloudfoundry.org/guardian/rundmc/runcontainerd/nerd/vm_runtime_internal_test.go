package nerd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"code.cloudfoundry.org/lager/v3/lagertest"
	"github.com/containerd/containerd/v2/client"
)

type fakeStdinCloser struct {
	closed chan struct{}
}

func (f *fakeStdinCloser) ID() string { return "process" }

func (f *fakeStdinCloser) CloseIO(context.Context, ...client.IOCloserOpts) error {
	close(f.closed)
	return nil
}

func TestNotifyOnStdinDoneWithoutStdin(t *testing.T) {
	processIO, done := notifyOnStdinDone(func() (io.Reader, io.Writer, io.Writer, bool) {
		return nil, io.Discard, nil, true
	})
	if done != nil {
		t.Fatal("expected no done channel without stdin")
	}
	stdin, stdout, _, tty := processIO()
	if stdin != nil || stdout != io.Discard || !tty {
		t.Fatal("expected the process IO to be passed through")
	}
}

func TestCloseStdinOnlyAfterStdinIsDone(t *testing.T) {
	processIO, done := notifyOnStdinDone(func() (io.Reader, io.Writer, io.Writer, bool) {
		return strings.NewReader("input"), nil, nil, false
	})
	process := &fakeStdinCloser{closed: make(chan struct{})}
	closeStdinWhenDone(context.Background(), lagertest.NewTestLogger("test"), process, "handle", done)

	stdin, _, _, _ := processIO()
	buf := make([]byte, 5)
	if _, err := stdin.Read(buf); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.closed:
		t.Fatal("stdin was closed before it was fully read")
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := stdin.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	select {
	case <-process.closed:
	case <-time.After(time.Second):
		t.Fatal("stdin was not closed after EOF")
	}
}

func TestRetryWhileNotStopped(t *testing.T) {
	calls := 0
	err := retryWhileNotStopped(lagertest.NewTestLogger("test"), func() error {
		calls++
		if calls < 3 {
			return errors.New("failed to delete task: Container not ready or stopped, impossible to delete")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("expected success after 3 calls, got %v after %d", err, calls)
	}

	calls = 0
	err = retryWhileNotStopped(lagertest.NewTestLogger("test"), func() error {
		calls++
		return errors.New("failed to delete task: do delete: failed to find container sandbox-envoy")
	})
	if err != nil || calls != 1 {
		t.Fatalf("expected a container already gone from the sandbox to count as deleted, got %v after %d", err, calls)
	}

	calls = 0
	err = retryWhileNotStopped(lagertest.NewTestLogger("test"), func() error {
		calls++
		return errors.New("other error")
	})
	if err == nil || calls != 1 {
		t.Fatalf("expected other errors not to be retried, got %v after %d", err, calls)
	}
}
