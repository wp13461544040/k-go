package proxy

// SSE write-error tracking.
//
// Streaming handlers write chunks to the client with fmt.Fprint/Flush and
// historically discarded the returned error. When the client goes away
// mid-stream (user abort, laptop sleep, network drop, CDN idle timeout) every
// subsequent write fails, but the handler keeps draining the upstream model and
// finally logs the request as a SUCCESS. The admin panel then shows
// "everything is fine" while the user watches the conversation stop dead —
// exactly the "看不到失败记录，对话直接结束" symptom.
//
// writeTrackingWriter records the first write/flush error so handlers can
// classify the request as a client disconnect instead of a success.

import (
	"net/http"
	"sync/atomic"
)

type writeTrackingWriter struct {
	http.ResponseWriter
	writeErr atomic.Value // string: first error observed
}

func (w *writeTrackingWriter) rememberErr(err error) {
	if err == nil {
		return
	}
	w.writeErr.CompareAndSwap(nil, err.Error())
}

func (w *writeTrackingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.rememberErr(err)
	return n, err
}

// Flush forwards to the underlying writer and records a flush error (the
// Go 1.20+ ResponseController surfaces client-gone errors that a bare
// http.Flusher.Flush() call swallows).
func (w *writeTrackingWriter) Flush() {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		w.rememberErr(err)
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *writeTrackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WriteError returns the first write/flush error observed, or nil.
func (w *writeTrackingWriter) WriteError() error {
	if v := w.writeErr.Load(); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return &clientWriteError{msg: s}
		}
	}
	return nil
}

// clientWriteError is a tiny error type so call sites can tell a client
// disconnect apart from an upstream failure with errors.As.
type clientWriteError struct{ msg string }

func (e *clientWriteError) Error() string { return "client closed connection: " + e.msg }
