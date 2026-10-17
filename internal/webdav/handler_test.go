package webdav

import "testing"

func TestParseRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		header      string
		size        int64
		start       int64
		length      int64
		partial     bool
		satisfiable bool
	}{
		{name: "no range", header: "", size: 100, start: 0, length: 100, satisfiable: true},
		{name: "closed range", header: "bytes=10-19", size: 100, start: 10, length: 10, partial: true, satisfiable: true},
		{name: "open range", header: "bytes=90-", size: 100, start: 90, length: 10, partial: true, satisfiable: true},
		{name: "suffix range", header: "bytes=-5", size: 100, start: 95, length: 5, partial: true, satisfiable: true},
		{name: "suffix larger than file", header: "bytes=-500", size: 100, start: 0, length: 100, partial: true, satisfiable: true},
		{name: "range past end", header: "bytes=100-", size: 100, satisfiable: false},
		{name: "multi range falls back to full", header: "bytes=0-4,10-14", size: 100, start: 0, length: 100, satisfiable: true},
		{name: "invalid range", header: "bytes=abc-def", size: 100, satisfiable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			start, length, partial, satisfiable := parseRange(test.header, test.size)
			if satisfiable != test.satisfiable {
				t.Fatalf("satisfiable = %v, want %v", satisfiable, test.satisfiable)
			}
			if !satisfiable {
				return
			}
			if start != test.start || length != test.length || partial != test.partial {
				t.Fatalf("got (%d,%d,%v), want (%d,%d,%v)", start, length, partial, test.start, test.length, test.partial)
			}
		})
	}
}

func TestNormalizePath(t *testing.T) {
	t.Parallel()
	if got, err := normalizePath("/a%20b/c"); err != nil || got != "/a b/c" {
		t.Fatalf("normalize path = %q, %v", got, err)
	}
	if got, err := normalizePath("folder/./sub"); err != nil || got != "/folder/sub" {
		t.Fatalf("normalize path = %q, %v", got, err)
	}
	if _, err := normalizePath("/a%00b"); err == nil {
		t.Fatal("expected a NUL byte to be rejected")
	}
	// Traversal is cleaned rather than escaping the drive root.
	if got, err := normalizePath("/a/../../etc"); err != nil || got != "/etc" {
		t.Fatalf("normalize path = %q, %v", got, err)
	}
}

func TestSplitPath(t *testing.T) {
	t.Parallel()
	parent, name := splitPath("/movies/clip.mkv")
	if parent != "/movies" || name != "clip.mkv" {
		t.Fatalf("splitPath = %q, %q", parent, name)
	}
	parent, name = splitPath("/clip.mkv")
	if parent != "/" || name != "clip.mkv" {
		t.Fatalf("splitPath = %q, %q", parent, name)
	}
}
