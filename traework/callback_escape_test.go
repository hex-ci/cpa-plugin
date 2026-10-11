package main

import (
	"net/url"
	"strings"
	"testing"
)

// The public callback route is unauthenticated and served from the gateway's own
// origin, so nothing a caller puts in the query string may reach the document as
// markup.
func TestCallbackPageEscapesTheFailureText(t *testing.T) {
	query := url.Values{}
	query.Set("error_msg", `<script>alert(document.domain)</script>`)

	page := handleCallbackResource(query, nil)
	if strings.Contains(page, "<script>") {
		t.Fatalf("the injected script survived into the page: %s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("the failure text was not escaped: %s", page)
	}
}

// The same text reaches the page through the loopback listener's failure path.
func TestWriteCallbackPageEscapes(t *testing.T) {
	rec := newRecordingWriter()
	writeCallbackPage(rec, "登录失败", `"><img src=x onerror=alert(1)>`)

	body := rec.body.String()
	if strings.Contains(body, "<img") {
		t.Fatalf("the injected tag survived into the page: %s", body)
	}
	// html.EscapeString renders the quote as &#34;.
	if !strings.Contains(body, "&#34;&gt;&lt;img") {
		t.Fatalf("quotes and brackets must both be escaped: %s", body)
	}
}
