package docker

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

// ReadLogsBounded decodes raw TTY text or Docker frames with an 8-byte header,
// returning at most limit bytes and whether the output was truncated. The limit
// must be nonnegative. It never allocates from the untrusted frame length and
// stops reading once truncation is detected. The caller owns the input stream.
func ReadLogsBounded(input io.Reader, limit int) (string, bool, error) {
	r := bufio.NewReader(input)
	header, err := r.Peek(8)
	multiplexed := err == nil && header[0] <= 2 && header[1] == 0 && header[2] == 0 && header[3] == 0
	if !multiplexed {
		data, err := io.ReadAll(io.LimitReader(r, int64(limit+1)))
		truncated := len(data) > limit
		if truncated {
			data = data[:limit]
		}
		return string(data), truncated, err
	}
	var output strings.Builder
	for {
		var h [8]byte
		_, err := io.ReadFull(r, h[:])
		if err == io.EOF {
			return output.String(), false, nil
		}
		if err != nil {
			return "", false, err
		}
		if h[0] > 2 || h[1] != 0 || h[2] != 0 || h[3] != 0 {
			return "", false, errors.New("invalid Docker log header")
		}
		n := int64(binary.BigEndian.Uint32(h[4:]))
		remaining := int64(limit - output.Len())
		if n > remaining {
			_, err = io.CopyN(&output, r, remaining)
			return output.String(), true, err
		}
		if _, err = io.CopyN(&output, r, n); err != nil {
			return "", false, err
		}
	}
}
