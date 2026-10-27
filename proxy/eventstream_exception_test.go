package proxy

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

type headerPair struct{ name, value string }

// awsFrame builds one AWS Event Stream message with the given string headers.
func awsFrame(t *testing.T, headers []headerPair, payload string) []byte {
	t.Helper()
	var hdr []byte
	for _, h := range headers {
		v := []byte(h.value)
		hdr = append(hdr, byte(len(h.name)))
		hdr = append(hdr, h.name...)
		hdr = append(hdr, 7) // string
		hdr = append(hdr, byte(len(v)>>8), byte(len(v)))
		hdr = append(hdr, v...)
	}
	body := []byte(payload)
	total := 12 + len(hdr) + len(body) + 4
	frame := make([]byte, 12, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(hdr)))
	frame = append(frame, hdr...)
	frame = append(frame, body...)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}

// An exception frame (:message-type=exception) used to be silently skipped,
// leaving the truncated stream looking like a successful completion.
func TestParseEventStreamReturnsUpstreamException(t *testing.T) {
	stream := bytes.NewReader(awsFrame(t, []headerPair{
		{":message-type", "exception"},
		{":exception-type", "ThrottlingException"},
	}, `{"message":"Rate exceeded"}`))

	err := parseEventStream(stream, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error for an upstream exception frame, got nil")
	}
	if !strings.Contains(err.Error(), "ThrottlingException") {
		t.Fatalf("error should name the exception type, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Rate exceeded") {
		t.Fatalf("error should carry the upstream message, got %q", err.Error())
	}
}

// A stream that closes before delivering any frame is a failure, not an empty
// successful answer.
func TestParseEventStreamEmptyStreamIsError(t *testing.T) {
	err := parseEventStream(bytes.NewReader(nil), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error for an empty event stream, got nil")
	}
	if !strings.Contains(err.Error(), "without any event") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A normal stream still terminates cleanly and forwards its text.
func TestParseEventStreamNormalStreamStillSucceeds(t *testing.T) {
	var out strings.Builder
	stream := bytes.NewReader(awsFrame(t, []headerPair{
		{":event-type", "assistantResponseEvent"},
		{":message-type", "event"},
	}, `{"content":"hello"}`))

	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if !isThinking {
				out.WriteString(text)
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != "hello" {
		t.Fatalf("expected forwarded text %q, got %q", "hello", out.String())
	}
}
