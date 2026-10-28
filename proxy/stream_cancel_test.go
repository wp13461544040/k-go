package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"kiro-go/config"
)

// A cancelled (client-gone) context must abort before any upstream request is
// made, so an abandoned turn does not keep an account busy for the next retry.
func TestCallKiroAPIWithContextCancelledSkipsUpstream(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	old := kiroHttpStore.Load()
	t.Cleanup(func() { kiroHttpStore.Store(old) })
	t.Cleanup(func() { InitKiroHttpClient("") })

	hit := false
	kiroHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hit = true
		return nil, errors.New("upstream must not be called")
	})})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := CallKiroAPIWithContext(ctx, &config.Account{ID: "acc-1", Email: "a@example.com"},
		&KiroPayload{}, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if hit {
		t.Fatal("upstream request was attempted even though the context was cancelled")
	}
}

// Cancelling mid-stream must surface as a cancellation error (the handler then
// classifies it as a client disconnect instead of an account failure), and must
// not fall through to the next endpoint.
func TestCallKiroAPIWithContextCancelStopsMidStream(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	old := kiroHttpStore.Load()
	t.Cleanup(func() { kiroHttpStore.Store(old) })
	t.Cleanup(func() { InitKiroHttpClient("") })

	var calls int
	ctx, cancel := context.WithCancel(context.Background())
	kiroHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		// Cancel while the body is being consumed: the first read triggers it.
		body := &cancelOnReadBody{cancel: cancel}
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})})

	err := CallKiroAPIWithContext(ctx, &config.Account{ID: "acc-2", Email: "b@example.com",
		ProfileArn: "arn:aws:codewhisperer:profile/test"}, &KiroPayload{}, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error once the client context is cancelled")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cancel") {
		t.Fatalf("expected a cancellation error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 upstream attempt, got %d", calls)
	}
}

type cancelOnReadBody struct {
	cancel context.CancelFunc
	done   bool
}

func (b *cancelOnReadBody) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		b.cancel()
	}
	return 0, context.Canceled
}

func (b *cancelOnReadBody) Close() error { return nil }
