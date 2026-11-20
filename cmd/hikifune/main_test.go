package main

import "testing"

// HIKIFUNE_WATCH goes to the agents in whole seconds, so anything else is
// refused at the start rather than cut on the way.
func TestWatchSetting(t *testing.T) {
	for v, ok := range map[string]bool{"2m": true, "90s": true, "1s": true,
		"500ms": false, "1500ms": false, "0s": false, "-1m": false, "soon": false} {
		t.Setenv("HIKIFUNE_DATA", t.TempDir())
		t.Setenv("HIKIFUNE_WATCH", v)
		if _, err := runner(); (err == nil) != ok {
			t.Errorf("HIKIFUNE_WATCH=%s: %v", v, err)
		}
	}
}
