package devicesync

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestTailNameCanonicalOnly(t *testing.T) {
	h := syncproto.Sum([]byte("literal tail\n"))
	for _, name := range []string{"1-0", "9223372036854775807-3", "12-7-" + h.String()} {
		if _, ok := parseTailName(name); !ok {
			t.Fatalf("canonical name rejected: %s", name)
		}
	}
	for _, name := range []string{"0-0", "-1-0", "+1-0", "01-0", "1-00", "1--1", "1-9223372036854775808", "1-0-extra", "1-0-" + strings.ToUpper(h.String()), "1-0-" + h.String() + ".tmp", "1-0-" + h.String() + "-extra", "1-0/extra"} {
		if _, ok := parseTailName(name); ok {
			t.Fatalf("noncanonical name accepted: %s", name)
		}
	}
}

func TestTailVersionPreservesOldWithinCap(t *testing.T) {
	old, next := bytes.Repeat([]byte("o"), 20), bytes.Repeat([]byte("n"), 40)
	for _, cap := range []int64{59, 60} {
		t.Run(strconv.FormatInt(cap, 10), func(t *testing.T) {
			s := publicationSpool(t, cap)
			if err := s.PutTail(7, 0, old); err != nil {
				t.Fatal(err)
			}
			err := s.PutTailVersion(7, 0, syncproto.Sum(next), next)
			if cap == 59 {
				if !errors.Is(err, ErrSpoolFull) || s.Used() != 20 {
					t.Fatalf("cap did not preserve prior tail: used=%d err=%v", s.Used(), err)
				}
				body, found, err := s.TailVersion(7, 0, syncproto.Sum(old))
				if err != nil || !found || !bytes.Equal(body, old) {
					t.Fatal("cap refusal lost legacy committed bytes")
				}
				return
			}
			if err != nil || s.Used() != 60 {
				t.Fatalf("old and new were not accounted: %d %v", s.Used(), err)
			}
			if err := s.PutTailVersion(7, 0, syncproto.Sum(next), next); err != nil || s.Used() != 60 {
				t.Fatal("duplicate immutable publication changed accounting")
			}
			if body, ok, err := s.TailVersion(7, 0, syncproto.Sum(next)); err != nil || !ok || !bytes.Equal(body, next) {
				t.Fatal("new immutable tail not readable")
			}
			if err := s.DropTailVersion(7, 0, syncproto.Sum(old)); err != nil || s.Used() != 40 {
				t.Fatal("released legacy version not accounted")
			}
			if body, ok, err := s.TailVersion(7, 0, syncproto.Sum(next)); err != nil || !ok || !bytes.Equal(body, next) {
				t.Fatal("old cleanup removed new immutable tail")
			}
		})
	}
}

func TestTailVersionVerifiedFallbackAndMismatches(t *testing.T) {
	s := publicationSpool(t, 1024)
	body := []byte("literal original bytes\n")
	h := syncproto.Sum(body)
	if err := s.PutTail(3, 0, body); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.TailVersion(3, 0, h); err != nil || !found {
		t.Fatal("verified legacy fallback failed")
	}
	if err := s.PutTailVersion(3, 0, h, []byte("different")); err == nil {
		t.Fatal("mismatching publication accepted")
	}
	if _, _, err := s.TailVersion(3, 0, syncproto.Sum([]byte("different"))); err == nil {
		t.Fatal("mismatching legacy fallback accepted")
	}
	name, _ := tailVersionName(3, 0, h)
	if err := os.WriteFile(filepath.Join(s.dir, "tails", name), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TailVersion(3, 0, h); err == nil {
		t.Fatal("corrupt expected immutable candidate fell back to legacy")
	}
	if err := s.PutTailVersion(3, 0, h, body); err == nil {
		t.Fatal("immutable corruption silently reused")
	}
	if err := s.DropTailVersion(3, 0, h); err == nil {
		t.Fatal("immutable corruption blindly removed")
	}
}

func TestTailVersionDropPreservesNewCanonical(t *testing.T) {
	s := publicationSpool(t, 1024)
	old, next := []byte("old\n"), []byte("next\n")
	if err := s.PutTailVersion(1, 0, syncproto.Sum(old), old); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTail(1, 0, next); err != nil {
		t.Fatal(err)
	}
	if err := s.DropTailVersion(1, 0, syncproto.Sum(old)); err != nil {
		t.Fatal(err)
	}
	if s.Used() != int64(len(next)) {
		t.Fatal("cleanup charged or deleted newer canonical")
	}
	if body, ok, err := s.Tail(1, 0); err != nil || !ok || !bytes.Equal(body, next) {
		t.Fatal("newer canonical removed")
	}
}

func TestTailVersionRejectsNonregularCandidates(t *testing.T) {
	s := publicationSpool(t, 1024)
	body := []byte("literal\n")
	h := syncproto.Sum(body)
	name, _ := tailVersionName(1, 0, h)
	outside := filepath.Join(t.TempDir(), "tail")
	if err := os.WriteFile(outside, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.dir, "tails", name)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TailVersion(1, 0, h); err == nil {
		t.Fatal("symlink candidate read")
	}
	if err := s.PutTailVersion(1, 0, h, body); err == nil {
		t.Fatal("symlink candidate accepted for publication")
	}
	if err := s.DropTailVersion(1, 0, h); err == nil {
		t.Fatal("symlink candidate removed")
	}
}
