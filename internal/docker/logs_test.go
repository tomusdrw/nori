package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLogsBounded(t *testing.T) {
	var framed bytes.Buffer
	for _, part := range []string{"hello\n", "world\n"} {
		var h [8]byte
		h[0] = 1
		binary.BigEndian.PutUint32(h[4:], uint32(len(part)))
		framed.Write(h[:])
		framed.WriteString(part)
	}
	for _, tc := range []struct {
		name, input, want string
		limit             int
		truncated         bool
	}{
		{"raw", "hello world", "hello", 5, true},
		{"multiplexed", framed.String(), "hello\nworld\n", 100, false},
		{"bounded frame", framed.String(), "hello\nw", 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated, err := ReadLogsBounded(strings.NewReader(tc.input), tc.limit)
			if err != nil || got != tc.want || truncated != tc.truncated {
				t.Fatalf("got %q %t %v", got, truncated, err)
			}
		})
	}
}

func TestReadLogsBoundedFrameErrors(t *testing.T) {
	frame := func(length uint32, payload string) string {
		var header [8]byte
		header[0] = 1
		binary.BigEndian.PutUint32(header[4:], length)
		return string(header[:]) + payload
	}
	for _, tc := range []struct {
		name, input, want string
		limit             int
		truncated         bool
		wantErr           error
	}{
		{"short header", frame(1, "a") + "short", "", 10, false, io.ErrUnexpectedEOF},
		{"short payload", frame(5, "abc"), "", 10, false, io.EOF},
		{"short truncated payload", frame(5, "ab"), "ab", 3, true, io.EOF},
		{"large declared frame", frame(^uint32(0), "abc"), "abc", 3, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated, err := ReadLogsBounded(strings.NewReader(tc.input), tc.limit)
			if got != tc.want || truncated != tc.truncated || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %q %t %v", got, truncated, err)
			}
		})
	}
	invalid := frame(1, "a") + "\x03\x00\x00\x00\x00\x00\x00\x00"
	if _, _, err := ReadLogsBounded(strings.NewReader(invalid), 10); err == nil {
		t.Fatal("accepted an invalid frame header")
	}
}
