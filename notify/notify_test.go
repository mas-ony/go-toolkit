package notify

// Tests for Run in both modes: the context send gets, its release, and
// the recover around send.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/rs/zerolog"
)

// key is the type of the context value the tests carry.
type key struct{}

// The context send gets keeps the request's values, outlives its
// cancellation, carries the deadline, and is released once send returns.
func TestRunContext(t *testing.T) {
	for _, async := range []bool{false, true} {
		parent, cancelParent := context.WithCancel(
			context.WithValue(context.Background(), key{}, "req-1"))
		cancelParent()

		done := make(chan context.Context, 1)
		start := time.Now()
		Run(parent, zerolog.Nop(), "test", async, time.Minute,
			func(ctx context.Context) {
				if ctx.Err() != nil {
					t.Errorf("async=%t: send got a cancelled context",
						async)
				}
				if ctx.Value(key{}) != "req-1" {
					t.Errorf("async=%t: the value was lost", async)
				}
				deadline, ok := ctx.Deadline()
				if d := deadline.Sub(start); !ok || d < time.Minute ||
					d > time.Minute+time.Second {
					t.Errorf("async=%t: deadline %v", async, deadline)
				}
				done <- ctx
			})

		select {
		case ctx := <-done:
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Errorf("async=%t: the context was not released", async)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("async=%t: send never ran", async)
		}
	}
}

// A sync Run returns once send has; an async one returns before.
func TestRunWaitsOnlyWhenSync(t *testing.T) {
	ran := false
	Run(context.Background(), zerolog.Nop(), "test", false, time.Second,
		func(context.Context) { ran = true })
	if !ran {
		t.Error("sync Run returned before send ran")
	}

	release := make(chan struct{})
	finished := make(chan struct{})
	Run(context.Background(), zerolog.Nop(), "test", true, time.Second,
		func(context.Context) {
			<-release
			close(finished)
		})
	close(release)
	<-finished
}

// A panic in send is logged under entity, with its value and the stack,
// and goes no further, in both modes.
func TestRunRecovers(t *testing.T) {
	for _, async := range []bool{false, true} {
		// The send logs from its own goroutine when async.
		var buf testutil.SyncBuffer
		done := make(chan struct{})
		Run(context.Background(), zerolog.New(&buf), "surat_masuk", async,
			time.Second, func(context.Context) {
				defer close(done)
				panic("boom")
			})
		<-done

		var line map[string]any
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := json.Unmarshal([]byte(buf.String()), &line); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("async=%t: no log line: %q", async, buf.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		stack, _ := line["stack"].(string)
		if line["level"] != "error" || line["panic"] != "boom" ||
			line["entity"] != "surat_masuk" || stack == "" ||
			line["message"] != "notification recovered from panic" {
			t.Errorf("async=%t: log = %v", async, line)
		}
	}
}
