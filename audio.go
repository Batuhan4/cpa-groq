package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

// audioKind is a container format Groq accepts. Groq validates the upload's
// file extension strictly, so every kind maps to one fixed file name.
type audioKind int

const (
	kindUnknown audioKind = iota
	kindOgg
	kindMP3
	kindWAV
	kindFLAC
	kindWebM
	kindM4A
	kindMP4
)

func (k audioKind) filename() string {
	switch k {
	case kindOgg:
		return "audio.ogg"
	case kindMP3:
		return "audio.mp3"
	case kindWAV:
		return "audio.wav"
	case kindFLAC:
		return "audio.flac"
	case kindWebM:
		return "audio.webm"
	case kindM4A:
		return "audio.m4a"
	case kindMP4:
		return "audio.mp4"
	default:
		return ""
	}
}

func (k audioKind) contentType() string {
	switch k {
	case kindOgg:
		return "audio/ogg"
	case kindMP3:
		return "audio/mpeg"
	case kindWAV:
		return "audio/wav"
	case kindFLAC:
		return "audio/flac"
	case kindWebM:
		return "audio/webm"
	case kindM4A:
		return "audio/mp4"
	case kindMP4:
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

func (k audioKind) String() string {
	name := k.filename()
	if name == "" {
		return "unknown"
	}
	return strings.TrimPrefix(name, "audio.")
}

// mimeKinds maps declared MIME types (parameters stripped, lower-cased) to a
// container. Telegram voice notes arrive as audio/ogg (Opus in Ogg, often
// saved as .oga); all Ogg flavours are uploaded as audio.ogg.
var mimeKinds = map[string]audioKind{
	"audio/ogg":                kindOgg,
	"audio/oga":                kindOgg,
	"audio/opus":               kindOgg,
	"audio/x-opus+ogg":         kindOgg,
	"audio/x-ogg":              kindOgg,
	"audio/vorbis":             kindOgg,
	"application/ogg":          kindOgg,
	"audio/mpeg":               kindMP3,
	"audio/mp3":                kindMP3,
	"audio/mpeg3":              kindMP3,
	"audio/x-mpeg-3":           kindMP3,
	"audio/x-mp3":              kindMP3,
	"audio/mpga":               kindMP3,
	"audio/wav":                kindWAV,
	"audio/x-wav":              kindWAV,
	"audio/wave":               kindWAV,
	"audio/vnd.wave":           kindWAV,
	"audio/x-pn-wav":           kindWAV,
	"audio/flac":               kindFLAC,
	"audio/x-flac":             kindFLAC,
	"audio/webm":               kindWebM,
	"video/webm":               kindWebM,
	"audio/mp4":                kindM4A,
	"audio/m4a":                kindM4A,
	"audio/x-m4a":              kindM4A,
	"audio/aac-mp4":            kindM4A,
	"video/mp4":                kindMP4,
	"audio/aac":                kindUnknown, // only accepted when the bytes are really MP4/M4A
	"audio/x-aac":              kindUnknown,
	"application/octet-stream": kindUnknown,
	"":                         kindUnknown,
}

var errNotAudio = errors.New("the first inline_data part is not audio")
var errUnsupportedAudio = errors.New("unsupported audio format; send ogg/opus, mp3, wav, flac, webm or m4a/mp4")

func normalizeMIME(raw string) string {
	mime := strings.ToLower(strings.TrimSpace(raw))
	if idx := strings.IndexByte(mime, ';'); idx >= 0 {
		mime = strings.TrimSpace(mime[:idx])
	}
	return mime
}

// resolveAudioKind decides which container the upload is. The bytes win when
// they carry a recognisable signature (the upload name must match the real
// content), the declared MIME type is the fallback for formats without a
// reliable signature at offset 0.
func resolveAudioKind(declaredMIME string, audio []byte) (audioKind, error) {
	mime := normalizeMIME(declaredMIME)
	declared, known := mimeKinds[mime]
	if !known {
		switch {
		case strings.HasPrefix(mime, "audio/"):
			// An audio type we have no mapping for: rely on the signature.
			declared = kindUnknown
		default:
			return kindUnknown, errNotAudio
		}
	}
	if sniffed := sniffAudio(audio); sniffed != kindUnknown {
		if declared == kindMP4 && sniffed == kindM4A {
			return kindMP4, nil
		}
		return sniffed, nil
	}
	if declared != kindUnknown {
		return declared, nil
	}
	return kindUnknown, errUnsupportedAudio
}

// sniffAudio recognises container signatures at the start of the payload.
func sniffAudio(b []byte) audioKind {
	switch {
	case len(b) >= 4 && bytes.Equal(b[:4], []byte("OggS")):
		return kindOgg
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WAVE")):
		return kindWAV
	case len(b) >= 4 && bytes.Equal(b[:4], []byte("fLaC")):
		return kindFLAC
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return kindWebM
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")):
		return kindM4A
	case len(b) >= 3 && bytes.Equal(b[:3], []byte("ID3")):
		return kindMP3
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0 && b[1]&0x06 != 0:
		// MPEG audio frame sync with layer I/II/III bits set. ADTS AAC has
		// layer bits 00 and is deliberately not matched here.
		return kindMP3
	default:
		return kindUnknown
	}
}

// decodeBase64 decodes inline_data.data. Standard base64 is what Gemini
// clients send; URL-safe and unpadded variants and embedded line breaks are
// tolerated. maxDecoded bounds the output before any allocation happens.
func decodeBase64(s string, maxDecoded int64) ([]byte, error) {
	s = stripBase64Whitespace(s)
	if s == "" {
		return nil, errors.New("inline_data.data is empty")
	}
	if int64(base64.RawStdEncoding.DecodedLen(len(s))) > maxDecoded+3 {
		return nil, errTooLarge
	}
	enc := base64.StdEncoding
	urlSafe := strings.ContainsAny(s, "-_")
	padded := strings.HasSuffix(s, "=")
	switch {
	case urlSafe && (padded || len(s)%4 == 0):
		enc = base64.URLEncoding
	case urlSafe:
		enc = base64.RawURLEncoding
	case !padded && len(s)%4 != 0:
		enc = base64.RawStdEncoding
	}
	out, err := enc.DecodeString(s)
	if err != nil {
		return nil, errors.New("inline_data.data is not valid base64")
	}
	if int64(len(out)) > maxDecoded {
		return nil, errTooLarge
	}
	return out, nil
}

var errTooLarge = errors.New("audio exceeds the configured size limit")

func stripBase64Whitespace(s string) string {
	if !strings.ContainsAny(s, " \t\r\n") {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, s)
}

// statusForAudioError maps audio validation failures to client statuses.
// 400/413 are request faults for CPA and never cool down the credential.
func statusForAudioError(err error) int {
	if errors.Is(err, errTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
