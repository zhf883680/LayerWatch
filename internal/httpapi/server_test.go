package httpapi

import "testing"

func TestVideoContentType(t *testing.T) {
	tests := map[string]string{
		"print-1-20260910.mp4": "video/mp4",
		"PRINT.MP4":            "video/mp4",
		"clip.m4v":             "video/mp4",
		"clip.mov":             "video/quicktime",
		"clip.webm":            "video/webm",
		"clip.mkv":             "video/x-matroska",
		"notes.txt":            "application/octet-stream",
	}
	for path, want := range tests {
		if got := videoContentType(path); got != want {
			t.Fatalf("videoContentType(%q)=%q want %q", path, got, want)
		}
	}
}
