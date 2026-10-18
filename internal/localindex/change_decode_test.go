package localindex

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
)

func assertReplayDecode(t *testing.T, d *replayTextDecoder, frame []byte) string {
	t.Helper()
	want, wantErr := decompress(frame)
	got, gotErr := d.decode(frame)
	if got != want || reflect.TypeOf(gotErr) != reflect.TypeOf(wantErr) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || (wantErr != nil && !errors.Is(gotErr, wantErr)) {
		t.Fatalf("decode differs: text bytes=%d/%d error=%v/%v", len(got), len(want), gotErr, wantErr)
	}
	if cap(d.scratch) > replayScratchLimit {
		t.Fatalf("retained %d scratch bytes", cap(d.scratch))
	}
	return got
}

func TestReplayTextDecoderFrames(t *testing.T) {
	first, second := compress("first complete frame"), compress("second frame with output")
	concat := append(append([]byte{}, first...), second...)
	partial := append(append([]byte{}, first...), second[:len(second)-1]...)
	if text, err := decompress(partial); err == nil || !strings.HasPrefix(text, "first complete frame") {
		t.Fatal("fixture must produce partial output and error")
	}
	skip := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	for name, frame := range map[string][]byte{
		"nil": nil, "empty-frame": compress(""), "skippable": skip, "concatenated": concat,
		"truncated": second[:len(second)-1], "partial-second-frame": partial,
		"corrupt": []byte("not a zstd frame"), "valid-then-corrupt": append(append([]byte{}, first...), []byte("corrupt")...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var d replayTextDecoder
			saved := assertReplayDecode(t, &d, compress(strings.Repeat("old owned text", 4096)))
			assertReplayDecode(t, &d, frame)
			assertReplayDecode(t, &d, compress(strings.Repeat("overwrite", 8192)))
			if saved != strings.Repeat("old owned text", 4096) {
				t.Fatal("later decode changed returned text")
			}
		})
	}
}

func TestReplayTextDecoderLargeRows(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	noisy := make([]byte, replayScratchLimit+65537)
	for i := range noisy {
		noisy[i] = byte(rng.Uint32())
	}
	for name, large := range map[string]string{
		"compressible":   strings.Repeat("large output", replayScratchLimit/6),
		"incompressible": string(noisy),
	} {
		t.Run(name, func(t *testing.T) {
			var d replayTextDecoder
			before := assertReplayDecode(t, &d, compress(strings.Repeat("before", 4096)))
			big := assertReplayDecode(t, &d, compress(large))
			if big != large || len(big) <= replayScratchLimit {
				t.Fatal("large row was truncated")
			}
			if cap(d.scratch) != 0 {
				t.Fatal("oversized backing buffer retained")
			}
			after := assertReplayDecode(t, &d, compress(strings.Repeat("after", 4096)))
			if before != strings.Repeat("before", 4096) || big != large || after != strings.Repeat("after", 4096) {
				t.Fatal("small/large/small ownership changed")
			}
			// A short/empty decode must not retain an already oversized backing array.
			d.scratch = make([]byte, 0, replayScratchLimit+1)
			assertReplayDecode(t, &d, compress(""))
			if cap(d.scratch) != 0 {
				t.Fatal("empty output retained oversized capacity")
			}
		})
	}
}

func TestReplayTextDecoderEvidenceOwnership(t *testing.T) {
	call := func(id, command string, ordinal int64) transcript.Message {
		return transcript.Message{Kind: transcript.KindToolCall, ToolName: "Bash", ToolCallID: id, NativeID: id, Ordinal: ordinal, Text: command}
	}
	result := func(id, text string, ordinal int64) transcript.Message {
		return transcript.Message{Kind: transcript.KindToolResult, ToolCallID: id, NativeID: fmt.Sprint(ordinal), Ordinal: ordinal, Text: text}
	}
	hash := "abcdef01234567890123456789012345678901234"
	input := []transcript.Message{
		call("commit", `{"command":"git -C /owned-path commit -qm fix"}`, 0),
		result("commit", "", 1),
		call("reveal", `{"command":"git -C /owned-path rev-parse HEAD"}`, 2),
		result("unrelated", strings.Repeat("overwrite", 8192), 3),
		result("reveal", hash, 4),
		result("unrelated", strings.Repeat("replace", 8192), 5),
	}
	var d replayTextDecoder
	var state digest.CommitEvidenceState
	var decoded []transcript.Message
	var events []digest.CommitEvidence
	for i, m := range input {
		m.Text = assertReplayDecode(t, &d, compress(m.Text))
		decoded = append(decoded, m)
		events = append(events, state.Step(m)...)
		if want := digest.ExtractCommitEvidence(input[:i+1]); !reflect.DeepEqual(events, want) {
			t.Fatalf("pending state/evidence differs at row %d", i)
		}
		if !reflect.DeepEqual(decoded, input[:i+1]) {
			t.Fatalf("retained full-replay messages changed at row %d", i)
		}
	}
	if len(events) != 1 || events[0].Hash != hash || events[0].CommandDirectory != "/owned-path" {
		t.Fatalf("missing nonzero reveal evidence: %+v", events)
	}
}

// Each arm decodes the same frozen frames and retains every text until the
// replay ends, then reads every byte. Compression is outside the measurement.
func BenchmarkReplayTextDecoderExactWork(b *testing.B) {
	const rows = 32
	frames := make([][]byte, rows)
	for i := range frames {
		frames[i] = compress(strings.Repeat(fmt.Sprintf("row-%02d payload ", i), 512))
	}
	for _, reuse := range []bool{false, true} {
		name := "baseline"
		if reuse {
			name = "scratch"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var d replayTextDecoder
				var texts [rows]string
				for j, frame := range frames {
					var err error
					if reuse {
						texts[j], err = d.decode(frame)
					} else {
						texts[j], err = decompress(frame)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				checksum := uint64(0)
				for _, text := range texts {
					for k := range text {
						checksum += uint64(text[k])
					}
				}
				if checksum != 21305344 {
					b.Fatal("unexpected decoded workload", checksum)
				}
			}
		})
	}
}
