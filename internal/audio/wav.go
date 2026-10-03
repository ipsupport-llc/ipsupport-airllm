package audio

import "encoding/binary"

// WAVDuration reads a RIFF/WAVE file's length in seconds from its header:
// the data chunk's size over the format chunk's byte rate. ok is false for
// anything it cannot read that way — another container, a truncated header,
// a zero byte rate.
func WAVDuration(b []byte) (seconds float64, ok bool) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, false
	}
	var byteRate uint32
	for off := 12; off+8 <= len(b); {
		id, size := string(b[off:off+4]), binary.LittleEndian.Uint32(b[off+4:off+8])
		body := off + 8
		switch id {
		case "fmt ":
			if body+12 > len(b) {
				return 0, false
			}
			byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
		case "data":
			if byteRate == 0 {
				return 0, false
			}
			// A streaming writer leaves the size unknown (0 or all ones);
			// what is actually there is then the honest length.
			if avail := uint32(len(b) - body); size == 0 || size > avail {
				size = avail
			}
			return float64(size) / float64(byteRate), true
		}
		next := body + int(size) + int(size&1) // chunks are word-aligned
		if next <= off {
			return 0, false
		}
		off = next
	}
	return 0, false
}
