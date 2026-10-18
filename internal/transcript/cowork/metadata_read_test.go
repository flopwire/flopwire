package cowork

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetadataReadSizeChanges(t *testing.T) {
	data := []byte(`{"sessionId":"app"}`)
	for _, hint := range []int64{-1, maxMetadataBytes + 1} {
		if _, err := readMetadataBytes(bytes.NewReader(data), hint); err == nil {
			t.Fatalf("accepted invalid size hint %d", hint)
		}
	}
	for _, hint := range []int64{0, 1, int64(len(data) - 1), int64(len(data)), int64(len(data) + 20)} {
		got, err := readMetadataBytes(bytes.NewReader(data), hint)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size hint %d: %q, %v", hint, got, err)
		}
	}
	for _, hint := range []int64{0, maxMetadataBytes - 1, maxMetadataBytes} {
		got, err := readMetadataBytes(strings.NewReader(strings.Repeat(" ", maxMetadataBytes)), hint)
		if err != nil || len(got) != maxMetadataBytes {
			t.Fatalf("at limit, hint %d: len=%d, %v", hint, len(got), err)
		}
		_, err = readMetadataBytes(strings.NewReader(strings.Repeat(" ", maxMetadataBytes+1)), hint)
		if err == nil {
			t.Fatalf("accepted growth past limit, hint %d", hint)
		}
	}
}

type failingMetadataReader struct{}

func (failingMetadataReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestMetadataReadFailure(t *testing.T) {
	if _, err := readMetadataBytes(failingMetadataReader{}, 5); err == nil {
		t.Fatal("ignored read failure")
	}
}

func TestMetadataFreshReadAndJSONBoundary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.json")
	data, err := json.Marshal(goodMetadata("app"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		suffix string
		known  bool
	}{
		{"", true}, {" \n\t", true}, {" {}", false}, {" trailing", false}, {"\x00", false}, {"", true},
	} {
		put(t, path, string(data)+tc.suffix)
		_, m := readMetadata(root, path, "app")
		if m.Known() != tc.known {
			t.Fatalf("suffix %q: %+v", tc.suffix, m)
		}
	}
	put(t, path, string(data)+strings.Repeat(" ", maxMetadataBytes-len(data)))
	if _, m := readMetadata(root, path, "app"); !m.Known() {
		t.Fatalf("exact limit: %+v", m)
	}
	put(t, path, string(data)+strings.Repeat(" ", maxMetadataBytes-len(data)+1))
	if _, m := readMetadata(root, path, "app"); m.Known() {
		t.Fatal("accepted oversized whitespace suffix")
	}
	put(t, path, `{"sessionId":"app","cliSessionId":"`+nativeID+`","userSelectedFolders":["/host/new"]}`)
	_, m := readMetadata(root, path, "app")
	if !m.Known() || len(m.HostPaths()) != 1 || m.HostPaths()[0] != "/host/new" {
		t.Fatalf("stale mapping: %+v", m)
	}
}
