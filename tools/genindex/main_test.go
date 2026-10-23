package main

import "testing"

func TestAudioPermissionIsPreservedInTheIndex(t *testing.T) {
	for _, value := range []string{"yes", "no", ""} {
		m := parseINI([]byte("[needs]\naudio = " + value + "\nnetwork = no\ndocuments = none\n"))
		want := ""
		if value == "yes" {
			want = "audio=yes"
		}
		if got := needs(m); got != want {
			t.Errorf("audio = %q: needs %q, want %q", value, got, want)
		}
	}
}
