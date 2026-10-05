package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func batchTestSink(t *testing.T) *sink {
	t.Helper()
	e := newEnv(t)
	id := refreshedSource(t, e)
	var src source
	if err := e.pool.QueryRow(e.ctx, `SELECT s.id::text,s.device_id::text,d.user_id::text,s.agent FROM sources s JOIN devices d ON d.id=s.device_id WHERE s.id=$1`, id).
		Scan(&src.id, &src.deviceID, &src.userID, &src.agent); err != nil {
		t.Fatal(err)
	}
	src.generation, src.parseAttempt = 1, 1000
	s := newSink(e.ctx, e.pool, src)
	s.Conversation(&transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "batch-test"})
	return s
}

func TestSinkFailedByteFlushKeepsPendingBatch(t *testing.T) {
	s := batchTestSink(t)
	text := strings.Repeat("x", (4<<20)+1)
	first := batchMessage("first", 1, text)
	if err := s.Message(first); err != nil {
		t.Fatal(err)
	}
	live := s.ctx
	ctx, cancel := context.WithCancel(live)
	cancel()
	s.ctx = ctx
	second := batchMessage("second", 2, text)
	if err := s.Message(second); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if s.written != 0 || len(s.msgs) != 1 || s.msgs[0] != first || s.textBytes != len(text) {
		t.Fatal("failed preflush changed the pending batch")
	}
	s.ctx = live
	if err := s.Message(second); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if s.written != 2 || s.textBytes != 0 {
		t.Fatalf("written=%d bytes=%d", s.written, s.textBytes)
	}
}

func TestSinkStillFlushesRowLimit(t *testing.T) {
	s := batchTestSink(t)
	for i := range sinkBatch {
		if err := s.Message(batchMessage(fmt.Sprint(i), int64(i), "small")); err != nil {
			t.Fatal(err)
		}
	}
	if s.written != sinkBatch || len(s.msgs) != 0 || s.textBytes != 0 {
		t.Fatalf("written=%d pending=%d bytes=%d", s.written, len(s.msgs), s.textBytes)
	}
}

func batchMessage(id string, ordinal int64, text string) *transcript.Message {
	m := &transcript.Message{SessionID: "batch-test", NativeID: id, Ordinal: ordinal, Kind: transcript.KindUser, Role: "user", Parser: "claude@1"}
	m.SetText(text, transcript.CapConfig{})
	return m
}

func TestSinkFlushesBytesBeforeRowLimit(t *testing.T) {
	s := batchTestSink(t)
	text := strings.Repeat("x", (4<<20)+1)
	if err := s.Message(batchMessage("first", 1, text)); err != nil {
		t.Fatal(err)
	}
	if err := s.Message(batchMessage("second", 2, text)); err != nil {
		t.Fatal(err)
	}
	if s.written != 1 || len(s.msgs) != 1 {
		t.Fatalf("crossing 8 MiB must flush the earlier message before retaining the next: written=%d pending=%d", s.written, len(s.msgs))
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if s.written != 2 {
		t.Fatalf("written=%d", s.written)
	}
}

func TestSinkWritesOversizedMessageAlone(t *testing.T) {
	s := batchTestSink(t)
	text := strings.Repeat("x", (8<<20)+1)
	if err := s.Message(batchMessage("large", 1, text)); err != nil {
		t.Fatal(err)
	}
	if s.written != 1 || len(s.msgs) != 0 {
		t.Fatalf("oversized message must flush immediately: written=%d pending=%d", s.written, len(s.msgs))
	}
	var got string
	if err := s.pool.QueryRow(s.ctx, `SELECT text FROM messages WHERE native_id='large' AND source_id=$1`, s.src.id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != text {
		t.Fatal("oversized message was truncated")
	}
}

func TestSinkReleasesFlushedMessages(t *testing.T) {
	s := batchTestSink(t)
	if err := s.Message(batchMessage("one", 1, "hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	for i, m := range s.msgs[:cap(s.msgs)] {
		if m != nil {
			t.Fatalf("flushed batch retains message %d", i)
		}
	}
}
