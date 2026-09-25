package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

var (
	sigOgg  = oggStub()
	sigWAV  = sineWAV(0.05, 440)
	sigFLAC = append([]byte("fLaC"), make([]byte, 32)...)
	sigWebM = append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 32)...)
	sigM4A  = append([]byte{0, 0, 0, 0x20}, append([]byte("ftypM4A "), make([]byte, 32)...)...)
	sigID3  = append([]byte("ID3\x04\x00"), make([]byte, 32)...)
	sigMP3  = append([]byte{0xFF, 0xFB, 0x90, 0x64}, make([]byte, 32)...)
	sigADTS = append([]byte{0xFF, 0xF1, 0x50, 0x80}, make([]byte, 32)...)
	noise   = bytes.Repeat([]byte{0x42, 0x13, 0x37}, 40)
)

func TestSniffAudio(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want audioKind
	}{
		{"ogg", sigOgg, kindOgg},
		{"wav", sigWAV, kindWAV},
		{"flac", sigFLAC, kindFLAC},
		{"webm", sigWebM, kindWebM},
		{"m4a", sigM4A, kindM4A},
		{"id3", sigID3, kindMP3},
		{"mp3 frame", sigMP3, kindMP3},
		{"adts aac is not mp3", sigADTS, kindUnknown},
		{"noise", noise, kindUnknown},
		{"empty", nil, kindUnknown},
		{"short", []byte("Og"), kindUnknown},
	}
	for _, c := range cases {
		if got := sniffAudio(c.in); got != c.want {
			t.Errorf("%s: sniffAudio = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResolveAudioKind(t *testing.T) {
	cases := []struct {
		name  string
		mime  string
		audio []byte
		want  audioKind
		err   error
	}{
		{"telegram voice", "audio/ogg", sigOgg, kindOgg, nil},
		{"oga alias", "audio/oga", sigOgg, kindOgg, nil},
		{"opus alias", "audio/opus", sigOgg, kindOgg, nil},
		{"mime params and case", "Audio/OGG; codecs=opus", sigOgg, kindOgg, nil},
		{"octet-stream sniffed", "application/octet-stream", sigOgg, kindOgg, nil},
		{"missing mime sniffed", "", sigWAV, kindWAV, nil},
		{"bytes beat a wrong label", "audio/mpeg", sigOgg, kindOgg, nil},
		{"mp3 without signature trusts mime", "audio/mpeg", noise, kindMP3, nil},
		{"wav label", "audio/x-wav", sigWAV, kindWAV, nil},
		{"flac", "audio/flac", sigFLAC, kindFLAC, nil},
		{"webm video label", "video/webm", sigWebM, kindWebM, nil},
		{"m4a", "audio/x-m4a", sigM4A, kindM4A, nil},
		{"mp4 video keeps mp4 name", "video/mp4", sigM4A, kindMP4, nil},
		{"aac label with mp4 bytes", "audio/aac", sigM4A, kindM4A, nil},
		{"raw adts aac", "audio/aac", sigADTS, kindUnknown, errUnsupportedAudio},
		{"unknown audio subtype sniffed", "audio/x-unknown", sigFLAC, kindFLAC, nil},
		{"unknown audio subtype noise", "audio/x-unknown", noise, kindUnknown, errUnsupportedAudio},
		{"image", "image/png", sigOgg, kindUnknown, errNotAudio},
		{"text", "text/plain", noise, kindUnknown, errNotAudio},
		{"octet-stream noise", "application/octet-stream", noise, kindUnknown, errUnsupportedAudio},
	}
	for _, c := range cases {
		got, err := resolveAudioKind(c.mime, c.audio)
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("%s: resolveAudioKind = %v, %v; want %v, %v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestUploadNames(t *testing.T) {
	want := map[audioKind]string{
		kindOgg: "audio.ogg", kindMP3: "audio.mp3", kindWAV: "audio.wav", kindFLAC: "audio.flac",
		kindWebM: "audio.webm", kindM4A: "audio.m4a", kindMP4: "audio.mp4", kindUnknown: "",
	}
	for k, name := range want {
		if k.filename() != name {
			t.Errorf("%d: filename %q want %q", k, k.filename(), name)
		}
	}
	if kindOgg.contentType() != "audio/ogg" || kindMP3.contentType() != "audio/mpeg" {
		t.Error("unexpected content types")
	}
}

func TestDecodeBase64Variants(t *testing.T) {
	data := []byte{0xfb, 0xff, 0xfe, 0x00, 0x01, 0x02, 0x03}
	variants := map[string]string{
		"std":     base64.StdEncoding.EncodeToString(data),
		"raw std": base64.RawStdEncoding.EncodeToString(data),
		"url":     base64.URLEncoding.EncodeToString(data),
		"raw url": base64.RawURLEncoding.EncodeToString(data),
		"wrapped": wrap76(base64.StdEncoding.EncodeToString(bytes.Repeat(data, 30))),
	}
	for name, enc := range variants {
		out, err := decodeBase64(enc, 1<<20)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "wrapped" {
			if !bytes.Equal(out, bytes.Repeat(data, 30)) {
				t.Fatalf("%s: mismatch", name)
			}
			continue
		}
		if !bytes.Equal(out, data) {
			t.Fatalf("%s: got %x", name, out)
		}
	}
}

func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

func TestDecodeBase64Errors(t *testing.T) {
	if _, err := decodeBase64("", 100); err == nil {
		t.Fatal("empty data must fail")
	}
	if _, err := decodeBase64("!!!not base64!!!", 100); err == nil {
		t.Fatal("garbage must fail")
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, 1000))
	if _, err := decodeBase64(big, 999); !errors.Is(err, errTooLarge) {
		t.Fatalf("expected errTooLarge, got %v", err)
	}
	if out, err := decodeBase64(big, 1000); err != nil || len(out) != 1000 {
		t.Fatalf("exact limit must pass: %d %v", len(out), err)
	}
	huge := strings.Repeat("A", 4_000_000)
	if _, err := decodeBase64(huge, 1000); !errors.Is(err, errTooLarge) {
		t.Fatalf("oversized input must be rejected before decoding, got %v", err)
	}
}
