package localindex

// replayScratchLimit bounds retained scratch capacity, not decoded row size.
// Larger rows still decode in full; their buffers are discarded after copying.
// One MiB is a bounded trial ceiling for ordinary tool output, not a tuned limit.
const replayScratchLimit = 1 << 20

// replayTextDecoder belongs to one invocation of replayChangeEvents. It must not
// escape into transaction checkpoints or be shared between concurrent callers.
type replayTextDecoder struct{ scratch []byte }

func (d *replayTextDecoder) decode(frame []byte) (string, error) {
	out, err := zdec.DecodeAll(frame, d.scratch[:0])
	// Preserve decompress's partial-output/error contract. The ordinary string
	// conversion copies the bytes: later decodes cannot mutate retained messages
	// or evidence substrings referring to this text.
	text := string(out)
	if err != nil || cap(out) > replayScratchLimit {
		d.scratch = nil
	} else {
		d.scratch = out[:0]
	}
	return text, err
}
