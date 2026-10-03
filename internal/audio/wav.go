package audio

import "encoding/binary"

// WAVDuration reads a RIFF/WAVE file's length in seconds from its header:
// the data chunk's size over the format chunk's byte rate. ok is false for
// anything it cannot read that way — another container, a truncated header,
// a zero byte rate.
func WAVDuration(b []byte) (seconds float64, ok bool) {
	h, ok := readWAVHeader(b)
	if !ok || h.byteRate == 0 {
		return 0, false
	}
	return float64(h.dataSize) / float64(h.byteRate), true
}

// WAVSampleRate reads a RIFF/WAVE file's sample rate from its format chunk.
// ok is false for anything that is not a WAV with a format chunk ahead of
// its data and a non-zero rate.
func WAVSampleRate(b []byte) (rate int, ok bool) {
	h, ok := readWAVHeader(b)
	if !ok || h.sampleRate == 0 {
		return 0, false
	}
	return int(h.sampleRate), true
}

// wavHeader is what the gateway reads from a WAV: the format chunk's rates
// and the data chunk's size.
type wavHeader struct {
	sampleRate, byteRate, dataSize uint32
}

// readWAVHeader walks the chunks up to the data chunk. The format chunk
// must come first, as every writer puts it.
func readWAVHeader(b []byte) (wavHeader, bool) {
	var h wavHeader
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return h, false
	}
	sawFormat := false
	for off := 12; off+8 <= len(b); {
		id, size := string(b[off:off+4]), binary.LittleEndian.Uint32(b[off+4:off+8])
		body := off + 8
		switch id {
		case "fmt ":
			if body+12 > len(b) {
				return h, false
			}
			h.sampleRate = binary.LittleEndian.Uint32(b[body+4 : body+8])
			h.byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
			sawFormat = true
		case "data":
			if !sawFormat {
				return h, false
			}
			// A streaming writer leaves the size unknown (0 or all ones);
			// what is actually there is then the honest length.
			if avail := uint32(len(b) - body); size == 0 || size > avail {
				size = avail
			}
			h.dataSize = size
			return h, true
		}
		next := body + int(size) + int(size&1) // chunks are word-aligned
		if next <= off {
			return h, false
		}
		off = next
	}
	return h, false
}
