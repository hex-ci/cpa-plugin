package main

import "testing"

func TestIsWorkbuddyAuthListNameIncludesLegacy(t *testing.T) {
	for _, name := range []string{"workbuddy.json", "workbuddy-u1.json", "WORKBUDDY-U2.JSON"} {
		if !isWorkbuddyAuthListName(name) {
			t.Fatalf("%s should be included", name)
		}
	}
	if isWorkbuddyAuthListName("other.json") {
		t.Fatal("other provider should not be included")
	}
}

func TestShouldDeleteLegacyForUIDRequiresMatchingUID(t *testing.T) {
	legacyA := []byte(`{"auth":{"accessToken":"a"},"account":{"uid":"A"}}`)
	if shouldDeleteLegacyForUID(legacyA, "B") {
		t.Fatal("must not delete legacy workbuddy.json for a different uid")
	}
	if !shouldDeleteLegacyForUID(legacyA, "A") {
		t.Fatal("should delete legacy workbuddy.json when uid matches migrated account")
	}
}
