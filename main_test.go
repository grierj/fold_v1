package main

import (
	"bytes"
	"io/fs"
	"os"
	"testing"
)

// internal/app's tests read these files from disk, so the directives that put
// them in the binary are checked here.
func TestEmbeddedFilesAreTheOnesOnDisk(t *testing.T) {
	want, err := os.ReadFile("docs/MEMBERS.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || !bytes.Equal(memberHelp, want) {
		t.Errorf("the embedded member guide is %d bytes, docs/MEMBERS.md is %d", len(memberHelp), len(want))
	}
	if _, err := fs.Stat(dist, "web/dist/README.txt"); err != nil {
		t.Errorf("the embedded web client has no web/dist/README.txt: %v", err)
	}
}
