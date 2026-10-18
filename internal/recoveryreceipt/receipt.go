// Package recoveryreceipt retains device-bound metadata needed to restrict
// already-uploaded recovery sources. A receipt cannot authorize new sharing.
package recoveryreceipt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

var ErrNotQualifying = errors.New("recoveryreceipt: prior recovery remains unreconciled: restriction proof unavailable")

var ErrAbsent = errors.New("recoveryreceipt: unreconciled prior recovery: no current-device restriction receipt")

type Binding struct {
	Server   string
	DeviceID string
}
type Receipt struct {
	Version         int                    `json:"version"`
	Server          string                 `json:"server"`
	DeviceID        string                 `json:"device_id"`
	NativeSessionID string                 `json:"native_session_id"`
	OriginalPath    string                 `json:"original_path"`
	Source          syncproto.PolicySource `json:"source"`
}

func NormalizeBinding(b Binding) (Binding, error) {
	server, err := client.NormalizeServer(b.Server)
	if err != nil || b.DeviceID == "" || strings.TrimSpace(b.DeviceID) != b.DeviceID || strings.ContainsAny(b.DeviceID, "\x00\t\n\r") {
		return Binding{}, errors.New("recoveryreceipt: invalid server/device binding")
	}
	b.Server = server
	return b, nil
}

func canonicalUUID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}
func absolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\t\n\r")
}
func validate(r Receipt, b Binding) error {
	if r.Version != 1 || r.Server != b.Server || r.DeviceID != b.DeviceID || !canonicalUUID(r.NativeSessionID) || !absolutePath(r.OriginalPath) || !absolutePath(r.Source.Path) || r.Source.Generation < 0 || strings.ContainsAny(r.Source.FileID, "\x00\t\n\r") {
		return errors.New("recoveryreceipt: invalid restriction provenance")
	}
	return nil
}

// SnapshotProvenance reads only the first conversation record of the private,
// checksum-verified snapshot. Callers must never pass an original export path.
// The record's native identity and original path must agree with its manifest.
func SnapshotProvenance(r io.Reader, entry cassimport.Entry) (string, error) {
	var record cassimport.Record
	if err := json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&record); err != nil {
		return "", errors.New("recoveryreceipt: invalid verified conversation header")
	}
	c := record.Conversation
	if record.Version != 1 || c == nil || record.Message != nil || c.Agent != entry.Agent || c.SessionID != entry.SessionID {
		return "", errors.New("recoveryreceipt: verified conversation identity differs from manifest")
	}
	if entry.Agent != transcript.AgentClaude || !canonicalUUID(entry.SessionID) {
		return "", ErrNotQualifying
	}
	recovered, ok := c.Extra["recovered_history"].(bool)
	external, externalOK := c.Extra["cass_external_id"].(string)
	original, pathOK := c.Extra["cass_source_path"].(string)
	if !ok || !recovered || !externalOK || external != entry.SessionID || !pathOK || !absolutePath(original) {
		return "", ErrNotQualifying
	}
	return original, nil
}

func bindingKey(b Binding) string {
	h := sha256.Sum256([]byte(b.Server + "\x00" + b.DeviceID))
	return hex.EncodeToString(h[:])
}

// openBoundDirectory never follows symlinks below the configured directory.
func openBoundDirectory(configDir string, b Binding, nativeUUID string, create bool) (int, error) {
	if create {
		if err := os.MkdirAll(configDir, 0700); err != nil {
			return -1, err
		}
	}
	fd, err := unix.Open(configDir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range []string{"recovery-policy-receipts", bindingKey(b), nativeUUID} {
		if create {
			if err = unix.Mkdirat(fd, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return -1, err
			}
			if err = unix.Fsync(fd); err != nil {
				unix.Close(fd)
				return -1, err
			}
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// Write publishes one immutable metadata receipt with an atomic durable rename.
// Independent receipts do not overwrite a shared index or lose concurrent writes.
func Write(configDir string, b Binding, r Receipt) error {
	var err error
	b, err = NormalizeBinding(b)
	if err != nil {
		return err
	}
	r.Version = 1
	r.Server = b.Server
	r.DeviceID = b.DeviceID
	if err = validate(r, b); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(raw) > 64<<10 {
		return errors.New("recoveryreceipt: restriction receipt exceeds metadata limit")
	}
	fd, err := openBoundDirectory(configDir, b, r.NativeSessionID, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	temporary := ".pending-" + uuid.NewString()
	fileFD, err := unix.Openat(fd, temporary, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(fd, temporary, 0)
	f := os.NewFile(uintptr(fileFD), temporary)
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	name := hex.EncodeToString(hash[:]) + ".json"
	if err = unix.Renameat(fd, temporary, fd, name); err != nil {
		return err
	}
	return unix.Fsync(fd)
}

// Read returns only exact-bound receipts for the requested native session.
// Absence is distinct from corruption and reports unreconciled prior recovery.
func Read(configDir string, b Binding, nativeUUID string) ([]Receipt, error) {
	var err error
	b, err = NormalizeBinding(b)
	if err != nil {
		return nil, err
	}
	if !canonicalUUID(nativeUUID) {
		return nil, errors.New("recoveryreceipt: invalid native UUID")
	}
	fd, err := openBoundDirectory(configDir, b, nativeUUID, false)
	if errors.Is(err, unix.ENOENT) {
		return nil, ErrAbsent
	}
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), "recovery-receipts")
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var result []Receipt
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".pending-") {
			continue
		}
		if len(name) != 69 || !strings.HasSuffix(name, ".json") {
			return nil, errors.New("recoveryreceipt: invalid receipt filename")
		}
		if _, err = hex.DecodeString(strings.TrimSuffix(name, ".json")); err != nil {
			return nil, errors.New("recoveryreceipt: invalid receipt filename")
		}
		fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(fileFD), name)
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			f.Close()
			return nil, errors.New("recoveryreceipt: unsafe receipt file")
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, 64<<10+1))
		closeErr := f.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		hash := sha256.Sum256(raw)
		if hex.EncodeToString(hash[:])+".json" != name {
			return nil, errors.New("recoveryreceipt: corrupt receipt digest")
		}
		var r Receipt
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&r); err != nil {
			return nil, errors.New("recoveryreceipt: malformed restriction receipt")
		}
		var trailing any
		if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, errors.New("recoveryreceipt: trailing restriction receipt data")
		}
		if err = validate(r, b); err != nil {
			return nil, err
		}
		if r.NativeSessionID != nativeUUID {
			return nil, errors.New("recoveryreceipt: receipt native identity differs from its directory")
		}
		result = append(result, r)
	}
	if len(result) == 0 {
		return nil, ErrAbsent
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Source.Path != result[j].Source.Path {
			return result[i].Source.Path < result[j].Source.Path
		}
		return result[i].Source.Generation < result[j].Source.Generation
	})
	return result, nil
}
