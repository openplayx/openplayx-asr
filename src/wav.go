package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

var errWavFormat = errors.New("audio_format_unsupported")

// wavDurationMs accepts only the normalised speech format (16 kHz, mono,
// 16-bit PCM) the App produces. Conversion is the client's controlled
// library step; this service never runs an external converter.
func wavDurationMs(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return 0, errWavFormat
	}
	fmtOK := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return 0, errWavFormat
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch string(ch[0:4]) {
		case "fmt ":
			var b [16]byte
			if size < 16 {
				return 0, errWavFormat
			}
			if _, err := io.ReadFull(f, b[:]); err != nil {
				return 0, errWavFormat
			}
			if binary.LittleEndian.Uint16(b[0:2]) != 1 || binary.LittleEndian.Uint16(b[2:4]) != 1 ||
				binary.LittleEndian.Uint32(b[4:8]) != 16000 || binary.LittleEndian.Uint16(b[14:16]) != 16 {
				return 0, errWavFormat
			}
			fmtOK = true
			if _, err := f.Seek(size-16+size&1, io.SeekCurrent); err != nil {
				return 0, errWavFormat
			}
		case "data":
			if !fmtOK {
				return 0, errWavFormat
			}
			pos, _ := f.Seek(0, io.SeekCurrent)
			avail := st.Size() - pos
			if size == 0 || size == 0xFFFFFFFF || size > avail {
				size = avail
			}
			return size / 2 * 1000 / 16000, nil
		default:
			if _, err := f.Seek(size+size&1, io.SeekCurrent); err != nil {
				return 0, errWavFormat
			}
		}
	}
}
