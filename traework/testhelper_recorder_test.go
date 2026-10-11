package main

import (
	"bytes"
	"net/http"
)

// newRecordingWriter captures what a handler writes, for the pages that are
// rendered straight into a response.
func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: http.Header{}}
}

type recordingWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *recordingWriter) Header() http.Header         { return w.header }
func (w *recordingWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *recordingWriter) WriteHeader(status int)      { w.status = status }
