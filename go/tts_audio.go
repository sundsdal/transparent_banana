package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"
	"time"
)

// Audio encodings the Gemini TTS models can return. The models choose the
// sample rate: 24 kHz for linear PCM and 8 kHz for G.711 μ-law and A-law.
const (
	encodingL16   = "l16"
	encodingMulaw = "mulaw"
	encodingAlaw  = "alaw"
)

type audioFormat struct {
	Encoding   string
	SampleRate int
	Channels   int
}

func (f audioFormat) requestMIME() string {
	return "audio/" + f.Encoding
}

func (f audioFormat) bytesPerSample() int {
	if f.Encoding == encodingL16 {
		return 2
	}
	return 1
}

func (f audioFormat) String() string {
	return fmt.Sprintf("%s %d Hz %s", strings.ToUpper(f.Encoding), f.SampleRate, map[int]string{1: "mono", 2: "stereo"}[f.Channels])
}

func (f audioFormat) duration(bytes int64) time.Duration {
	frame := int64(f.bytesPerSample() * f.Channels)
	if frame <= 0 || f.SampleRate <= 0 {
		return 0
	}
	return time.Duration(bytes / frame * int64(time.Second) / int64(f.SampleRate))
}

// silence returns d of digital silence in f. G.711 silence is not zero bytes.
func (f audioFormat) silence(d time.Duration) []byte {
	frames := int64(f.SampleRate) * int64(d) / int64(time.Second)
	data := make([]byte, frames*int64(f.Channels*f.bytesPerSample()))
	fill := byte(0)
	switch f.Encoding {
	case encodingMulaw:
		fill = 0xFF
	case encodingAlaw:
		fill = 0xD5
	}
	if fill != 0 {
		for i := range data {
			data[i] = fill
		}
	}
	return data
}

func defaultSampleRate(encoding string) int {
	if encoding == encodingL16 {
		return 24000
	}
	return 8000
}

// parseAudioMIME reads encodings such as "audio/l16; rate=24000; channels=1".
// It returns wav=true for RIFF responses, whose format comes from the header.
func parseAudioMIME(value string) (format audioFormat, wav bool, err error) {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return audioFormat{}, false, fmt.Errorf("unsupported audio MIME type %q", value)
	}
	switch mediaType {
	case "audio/wav", "audio/wave", "audio/x-wav", "audio/vnd.wave":
		return audioFormat{}, true, nil
	case "audio/l16", "audio/pcm":
		format.Encoding = encodingL16
	case "audio/mulaw", "audio/basic", "audio/pcmu":
		format.Encoding = encodingMulaw
	case "audio/alaw", "audio/pcma":
		format.Encoding = encodingAlaw
	default:
		return audioFormat{}, false, fmt.Errorf("unsupported audio MIME type %q", value)
	}
	format.SampleRate = defaultSampleRate(format.Encoding)
	format.Channels = 1
	if rate, err := strconv.Atoi(params["rate"]); err == nil && rate > 0 {
		format.SampleRate = rate
	}
	if channels, err := strconv.Atoi(params["channels"]); err == nil && channels > 0 {
		format.Channels = channels
	}
	return format, false, nil
}

const (
	wavFormatPCM   = 1
	wavFormatAlaw  = 6
	wavFormatMulaw = 7
)

// wavHeader builds a canonical header. A negative dataLen marks a stream of
// unknown length; most players accept the maximum sizes it then writes.
func wavHeader(f audioFormat, dataLen int64) []byte {
	var b bytes.Buffer
	unknown := dataLen < 0 || dataLen > 0xFFFFFFFF-64
	size := func(n int64) uint32 {
		if unknown {
			return 0xFFFFFFFF
		}
		return uint32(n)
	}
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	bytesPerSample := f.bytesPerSample()
	tag := uint16(wavFormatPCM)
	switch f.Encoding {
	case encodingMulaw:
		tag = wavFormatMulaw
	case encodingAlaw:
		tag = wavFormatAlaw
	}
	pcm := tag == wavFormatPCM
	riffSize := 36 + dataLen
	if !pcm {
		riffSize = 50 + dataLen
	}
	b.WriteString("RIFF")
	le(size(riffSize))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	if pcm {
		le(uint32(16))
	} else {
		le(uint32(18))
	}
	le(tag)
	le(uint16(f.Channels))
	le(uint32(f.SampleRate))
	le(uint32(f.SampleRate * f.Channels * bytesPerSample))
	le(uint16(f.Channels * bytesPerSample))
	le(uint16(bytesPerSample * 8))
	if !pcm {
		le(uint16(0)) // cbSize
		b.WriteString("fact")
		le(uint32(4))
		le(size(dataLen / int64(f.Channels)))
	}
	b.WriteString("data")
	le(size(dataLen))
	return b.Bytes()
}

// splitWAV extracts the sample data and format from a RIFF WAVE file.
func splitWAV(data []byte) (audioFormat, []byte, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return audioFormat{}, nil, errors.New("Gemini returned an invalid WAV file")
	}
	var format audioFormat
	haveFormat := false
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		body := offset + 8
		end := body + size
		if size < 0 || end > len(data) || end < body {
			if id == "data" {
				end = len(data) // Streamed WAVs may carry a placeholder size.
			} else {
				break
			}
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return audioFormat{}, nil, errors.New("Gemini returned a WAV file with an invalid format chunk")
			}
			tag := binary.LittleEndian.Uint16(data[body:])
			format.Channels = int(binary.LittleEndian.Uint16(data[body+2:]))
			format.SampleRate = int(binary.LittleEndian.Uint32(data[body+4:]))
			bits := binary.LittleEndian.Uint16(data[body+14:])
			switch {
			case tag == wavFormatPCM && bits == 16:
				format.Encoding = encodingL16
			case tag == wavFormatMulaw && bits == 8:
				format.Encoding = encodingMulaw
			case tag == wavFormatAlaw && bits == 8:
				format.Encoding = encodingAlaw
			default:
				return audioFormat{}, nil, fmt.Errorf("unsupported WAV encoding (format %d, %d bits)", tag, bits)
			}
			if format.Channels <= 0 || format.SampleRate <= 0 {
				return audioFormat{}, nil, errors.New("Gemini returned a WAV file with an invalid format chunk")
			}
			haveFormat = true
		case "data":
			if !haveFormat {
				return audioFormat{}, nil, errors.New("WAV data chunk precedes its format chunk")
			}
			return format, data[body:end], nil
		}
		offset = end + end%2 // Chunks are word aligned.
	}
	return audioFormat{}, nil, errors.New("Gemini returned a WAV file without audio data")
}

// audioSink writes a stream of same-format audio as raw samples or as WAV.
// When the destination can seek, finish rewrites the WAV header with exact sizes.
type audioSink struct {
	w       io.Writer
	wav     bool
	format  audioFormat
	started bool
	written int64
}

func (s *audioSink) write(format audioFormat, data []byte) error {
	if !s.started {
		s.format = format
		s.started = true
		if s.wav {
			if _, err := s.w.Write(wavHeader(format, -1)); err != nil {
				return fmt.Errorf("write audio: %w", err)
			}
		}
	} else if format != s.format {
		return fmt.Errorf("Gemini changed the audio format from %s to %s", s.format, format)
	}
	if len(data) == 0 {
		return nil
	}
	if _, err := s.w.Write(data); err != nil {
		return fmt.Errorf("write audio: %w", err)
	}
	s.written += int64(len(data))
	return nil
}

func (s *audioSink) writeSilence(d time.Duration) error {
	if !s.started || d <= 0 {
		return nil
	}
	return s.write(s.format, s.format.silence(d))
}

func (s *audioSink) finish() error {
	if !s.started || s.written == 0 {
		return errors.New("Gemini returned no audio")
	}
	if !s.wav {
		return nil
	}
	seeker, ok := s.w.(io.WriteSeeker)
	if !ok {
		return nil
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil // Pipes cannot seek; the placeholder header is still playable.
	}
	if _, err := seeker.Write(wavHeader(s.format, s.written)); err != nil {
		return fmt.Errorf("finalize WAV header: %w", err)
	}
	_, err := seeker.Seek(0, io.SeekEnd)
	return err
}
