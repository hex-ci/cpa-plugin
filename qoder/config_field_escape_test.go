package main

import (
	"html"
	"strings"
	"testing"
)

// The management UI escapes every config-field string with html.EscapeString
// before display, which turns an apostrophe into &#39; and makes the help text
// unreadable. That escaping is a host-side XSS guard the plugin cannot opt out
// of, so the plugin must not ship characters it would mangle: apostrophes and
// angle brackets.
//
// Reported as: the context_window help text showed "upstream&#39;s".
func TestConfigFieldDescriptionsAvoidHTMLEscapedChars(t *testing.T) {
	for _, field := range wbRegistration().Metadata.ConfigFields {
		escaped := html.EscapeString(field.Description)
		if escaped != field.Description {
			t.Errorf("config field %q description contains characters the host will escape:\n  raw:     %s\n  escaped: %s",
				field.Name, field.Description, escaped)
		}
		if strings.ContainsAny(field.Description, "'\"<>&") {
			t.Errorf("config field %q description contains HTML-sensitive characters: %q",
				field.Name, field.Description)
		}
	}
}

// Same rule for the display strings the host renders in the plugin list.
func TestRegistrationDisplayStringsAreHTMLEscapeSafe(t *testing.T) {
	meta := wbRegistration().Metadata
	values := map[string]string{
		"name":   meta.Name,
		"author": meta.Author,
	}
	for label, value := range values {
		if html.EscapeString(value) != value {
			t.Errorf("%s would be HTML-escaped by the host: %q", label, value)
		}
	}
}
