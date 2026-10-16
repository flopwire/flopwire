package cowork

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Unknown fields represent private data without retaining real user content.
func benchmarkMetadata(size int) []byte {
	prefix := `{"sessionId":"app","cliSessionId":"11111111-1111-4111-8111-111111111111","userSelectedFolders":["/host/repo"],"privatePayload":"`
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-2) + `"}`)
}

// Compare unchanged copies of this benchmark against the baseline and candidate
// production implementations; both include safe open, fresh read, and mapping.
func BenchmarkReadMetadata(b *testing.B) {
	for _, size := range []int{4096, 65536, 1048576, maxMetadataBytes} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			root := b.TempDir()
			path := filepath.Join(root, "app.json")
			if err := os.WriteFile(path, benchmarkMetadata(size), 0600); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for b.Loop() {
				_, m := readMetadata(root, path, "app")
				if !m.Known() {
					b.Fatal(m.Reason())
				}
			}
		})
	}
}

// Match the connected capture fixture: 31 metadata-only identities, unknown
// mappings, 30 files with 40,000 padding bytes and one with 8,500 padding bytes.
func BenchmarkDiscoverMetadataDistribution(b *testing.B) {
	root := b.TempDir()
	folder := filepath.Join(root, "account", "workspace")
	if err := os.MkdirAll(folder, 0700); err != nil {
		b.Fatal(err)
	}
	var total int64
	for i := range 31 {
		id := fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
		padding := 40000
		if i == 30 {
			padding = 8500
		}
		data, err := json.Marshal(map[string]any{
			"sessionId": "local_" + id, "cliSessionId": id,
			"cwd": "/sessions/synthetic", "userSelectedFolders": []string{},
			"syntheticPadding": strings.Repeat("x", padding),
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(folder, "local_"+id+".json"), data, 0600); err != nil {
			b.Fatal(err)
		}
		total += int64(len(data))
	}
	b.ReportAllocs()
	b.SetBytes(total)
	b.ResetTimer()
	for b.Loop() {
		result, err := Discover(root)
		if err != nil || result.MetadataOnly != 31 || len(result.IdentityLinks) != 31 {
			b.Fatalf("discover: %+v, %v", result, err)
		}
	}
}
