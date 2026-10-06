package recoveryreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

const receiptUUID = "11111111-2222-4333-8444-555555555555"

func testReceipt(gen int64) Receipt {
	return Receipt{NativeSessionID: receiptUUID, OriginalPath: "/gone/native.jsonl", Source: syncproto.PolicySource{Path: "/exports/recovery.jsonl", FileID: "", Generation: gen}}
}
func TestReceiptExactBindingAndConcurrentPublication(t *testing.T) {
	dir := t.TempDir()
	b := Binding{Server: "https://EXAMPLE.test:443/base/", DeviceID: "current-device"}
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- Write(dir, b, testReceipt(int64(i))) }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	refs, err := Read(dir, Binding{Server: "https://example.test/base", DeviceID: b.DeviceID}, receiptUUID)
	if err != nil || len(refs) != 12 {
		t.Fatalf("receipts=%+v,%v", refs, err)
	}
	for _, other := range []Binding{{"https://example.test/Base", b.DeviceID}, {"https://other.test/base", b.DeviceID}, {b.Server, "old-recovery-device"}} {
		if _, err = Read(dir, other, receiptUUID); !errors.Is(err, ErrAbsent) {
			t.Fatalf("other binding=%+v error=%v", other, err)
		}
	}
	for i, r := range refs {
		if r.Source.Generation != int64(i) || r.Source.FileID != "" {
			t.Fatalf("identity changed: %+v", r)
		}
	}
	path := filepath.Join(dir, "recovery-policy-receipts", bindingKey(Binding{"https://example.test/base", b.DeviceID}), receiptUUID)
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "content") {
			t.Fatalf("receipt contains nonmetadata: %s", raw)
		}
	}
}
func TestReceiptCorruptionAndSymlinkContainment(t *testing.T) {
	b := Binding{"https://example.test", "device"}
	for _, mode := range []string{"corrupt", "receipt symlink", "binding symlink", "root symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			normalized, _ := NormalizeBinding(b)
			if mode == "root symlink" {
				if err := os.Symlink(outside, filepath.Join(dir, "recovery-policy-receipts")); err != nil {
					t.Fatal(err)
				}
				if err := Write(dir, b, testReceipt(0)); err == nil {
					t.Fatal("followed root symlink")
				}
				return
			}
			if err := Write(dir, b, testReceipt(0)); err != nil {
				t.Fatal(err)
			}
			bound := filepath.Join(dir, "recovery-policy-receipts", bindingKey(normalized), receiptUUID)
			entries, _ := os.ReadDir(bound)
			path := filepath.Join(bound, entries[0].Name())
			switch mode {
			case "corrupt":
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "receipt symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(outside, "private")
				os.WriteFile(target, []byte("private unrelated contents"), 0600)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "binding symlink":
				if err := os.RemoveAll(bound); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, bound); err != nil {
					t.Fatal(err)
				}
				if err := Write(dir, b, testReceipt(1)); err == nil {
					t.Fatal("followed binding symlink")
				}
			}
			if _, err := Read(dir, b, receiptUUID); err == nil || errors.Is(err, ErrAbsent) {
				t.Fatalf("unsafe receipt accepted: %v", err)
			}
		})
	}
}
func TestReceiptBindingRejectsNonEquivalentURLs(t *testing.T) {
	for _, server := range []string{"https://user:credential@example.test", "https://example.test/?credential=secret", "https://example.test/a/../b", "https://example.test/a//b", "https://example.test/a%2Fb", "http://remote.test", "https://example.test/#fragment"} {
		if _, err := NormalizeBinding(Binding{server, "device"}); err == nil {
			t.Fatalf("unsafe URL accepted: %s", server)
		}
	}
	if _, err := Read(t.TempDir(), Binding{"https://example.test", "device"}, receiptUUID); !errors.Is(err, ErrAbsent) || !strings.Contains(err.Error(), "unreconciled prior recovery") {
		t.Fatalf("missing provenance diagnostic=%v", err)
	}
	raw, _ := json.Marshal(testReceipt(0))
	if strings.Contains(string(raw), "mapped") {
		t.Fatal("receipt grants mapping")
	}
}

func TestReceiptUnrelatedSessionCorruptionIsIsolated(t *testing.T) {
	dir := t.TempDir()
	binding := Binding{"https://example.test", "device"}
	if err := Write(dir, binding, testReceipt(0)); err != nil {
		t.Fatal(err)
	}
	other := testReceipt(0)
	other.NativeSessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if err := Write(dir, binding, other); err != nil {
		t.Fatal(err)
	}
	normalized, _ := NormalizeBinding(binding)
	otherDir := filepath.Join(dir, "recovery-policy-receipts", bindingKey(normalized), other.NativeSessionID)
	entries, err := os.ReadDir(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, entries[0].Name()), []byte("corrupt other session"), 0600); err != nil {
		t.Fatal(err)
	}
	receipts, err := Read(dir, binding, receiptUUID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("unrelated corruption blocked exact scope: %+v,%v", receipts, err)
	}
	if _, err := Read(dir, binding, other.NativeSessionID); err == nil || errors.Is(err, ErrAbsent) {
		t.Fatalf("relevant corruption accepted: %v", err)
	}
}

func TestReceiptRejectsTrailingDataAndMisplacedIdentity(t *testing.T) {
	for _, mode := range []string{"trailing", "misplaced native"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			binding := Binding{"https://example.test", "device"}
			normalized, _ := NormalizeBinding(binding)
			if err := Write(dir, binding, testReceipt(0)); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "recovery-policy-receipts", bindingKey(normalized), receiptUUID)
			entries, _ := os.ReadDir(target)
			path := filepath.Join(target, entries[0].Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "trailing" {
				raw = append(raw, []byte(" {}")...)
			} else {
				var r Receipt
				if err := json.Unmarshal(raw, &r); err != nil {
					t.Fatal(err)
				}
				r.NativeSessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
				raw, _ = json.Marshal(r)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(raw)
			if err := os.WriteFile(filepath.Join(target, hex.EncodeToString(hash[:])+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir, binding, receiptUUID); err == nil || errors.Is(err, ErrAbsent) {
				t.Fatalf("invalid exact-scope receipt accepted: %v", err)
			}
		})
	}
}
