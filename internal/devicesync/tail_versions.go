package devicesync

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// tailName recognizes only canonical legacy or immutable tail names. It does
// not accept temporary files, signed/zero-padded numbers or trailing text.
type tailName struct {
	sid, gen int64
	hash     syncproto.Hash
	version  bool
}

func parseTailName(name string) (tailName, bool) {
	var n tailName
	parts := strings.Split(name, "-")
	if len(parts) != 2 && len(parts) != 3 {
		return n, false
	}
	var err error
	if n.sid, err = strconv.ParseInt(parts[0], 10, 64); err != nil || n.sid <= 0 || strconv.FormatInt(n.sid, 10) != parts[0] {
		return tailName{}, false
	}
	if n.gen, err = strconv.ParseInt(parts[1], 10, 64); err != nil || n.gen < 0 || strconv.FormatInt(n.gen, 10) != parts[1] {
		return tailName{}, false
	}
	if len(parts) == 3 {
		if n.hash.UnmarshalText([]byte(parts[2])) != nil || n.hash.String() != parts[2] {
			return tailName{}, false
		}
		n.version = true
	}
	return n, true
}

func tailVersionName(sid, gen int64, hash syncproto.Hash) (string, error) {
	if sid <= 0 || gen < 0 {
		return "", errors.New("devicesync: invalid tail identity")
	}
	return fmt.Sprintf("%d-%d-%s", sid, gen, hash.String()), nil
}

// readTailCandidate rejects nonregular files before reading and bounds the read
// by the protocol's tail limit. The root keeps candidate opens inside tails.
func readTailCandidate(root *os.Root, name string) ([]byte, bool, error) {
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return nil, false, errors.New("devicesync: tail candidate is unavailable or nonregular")
	}
	if before.Size() > syncproto.MaxPartBytes {
		return nil, false, errors.New("devicesync: tail candidate exceeds protocol limit")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, false, errors.New("devicesync: cannot open tail candidate")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, false, errors.New("devicesync: tail candidate changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(f, syncproto.MaxPartBytes+1))
	if err != nil || int64(len(data)) != before.Size() {
		return nil, false, errors.New("devicesync: tail candidate changed or could not be read")
	}
	return data, true, nil
}

func openTailRoot(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("devicesync: spool directory unavailable")
	}
	defer root.Close()
	info, err := root.Lstat("tails")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("devicesync: tail directory unavailable or nonregular")
	}
	tails, err := root.OpenRoot("tails")
	if err != nil {
		return nil, errors.New("devicesync: cannot open tail directory")
	}
	return tails, nil
}

// PutTailVersion publishes immutable bytes while retaining earlier versions.
// Capture keeps the prior committed tail until its manifest transaction succeeds.
func (s *Spool) PutTailVersion(sid, gen int64, hash syncproto.Hash, data []byte) error {
	name, err := tailVersionName(sid, gen, hash)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > syncproto.MaxPartBytes || syncproto.Sum(data) != hash {
		return errors.New("devicesync: immutable tail bytes do not match hash or limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := openTailRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	old, found, err := readTailCandidate(root, name)
	if err != nil {
		return err
	}
	if found {
		if syncproto.Sum(old) != hash {
			return errors.New("devicesync: existing immutable tail does not match hash")
		}
		return nil
	}
	if err := s.checkSpaceLocked(int64(len(data))); err != nil {
		return err
	}
	retained, err := publishTailVersion(root, name, data)
	if err != nil {
		s.used += retained
		return err
	}
	s.used += int64(len(data))
	s.blocked = false
	return nil
}

// TailVersion first reads the expected immutable version, then a verified
// legacy canonical file. A mismatching existing candidate is an error.
func (s *Spool) TailVersion(sid, gen int64, hash syncproto.Hash) ([]byte, bool, error) {
	name, err := tailVersionName(sid, gen, hash)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := openTailRoot(s.dir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	for _, candidate := range []string{name, fmt.Sprintf("%d-%d", sid, gen)} {
		data, found, err := readTailCandidate(root, candidate)
		if err != nil {
			return nil, false, err
		}
		if found {
			if syncproto.Sum(data) != hash {
				return nil, false, errors.New("devicesync: tail candidate does not match committed hash")
			}
			return data, true, nil
		}
	}
	return nil, false, nil
}

// DropTailVersion removes the named immutable version and a legacy canonical
// only when its bytes match that hash. Callers must first durably release every
// reference to the version; this method does not inspect sync state.
func (s *Spool) DropTailVersion(sid, gen int64, hash syncproto.Hash) error {
	name, err := tailVersionName(sid, gen, hash)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := openTailRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var remove []struct {
		name string
		size int64
	}
	for _, candidate := range []string{name, fmt.Sprintf("%d-%d", sid, gen)} {
		data, found, err := readTailCandidate(root, candidate)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if syncproto.Sum(data) != hash {
			if candidate == name {
				return errors.New("devicesync: refusing to remove mismatching immutable tail")
			}
			continue // the canonical now represents another committed version
		}
		remove = append(remove, struct {
			name string
			size int64
		}{candidate, int64(len(data))})
	}
	for _, file := range remove {
		if err := root.Remove(file.name); err != nil {
			return errors.New("devicesync: cannot remove released tail version")
		}
		s.used -= file.size
	}
	return nil
}

// Publication stays in the opened tail directory, including rename and failed
// temporary cleanup. It preserves the spool owner's cap/accounting contract.
func publishTailVersion(root *os.Root, name string, data []byte) (retained int64, err error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, err
	}
	tmp := name + "-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, errors.New("devicesync: cannot create immutable tail temporary")
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			if cleanupErr := root.Remove(tmp); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				info, statErr := root.Lstat(tmp)
				if statErr == nil {
					retained = info.Size()
				} else if !errors.Is(statErr, os.ErrNotExist) {
					retained = int64(len(data))
				}
				err = errors.Join(err, errors.New("devicesync: immutable tail temporary cleanup failed"))
			}
		}
	}()
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	return 0, err
}
