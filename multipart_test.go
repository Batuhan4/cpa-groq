package main

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

func parseForm(t *testing.T, body []byte, contentType string) (map[string]string, *multipart.Part, []byte) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("content type %q: %v", contentType, err)
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string]string{}
	var filePart *multipart.Part
	var fileBytes []byte
	for {
		part, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == "file" {
			filePart, fileBytes = part, data
			continue
		}
		fields[part.FormName()] = string(data)
	}
	return fields, filePart, fileBytes
}

func TestBuildMultipartAllFields(t *testing.T) {
	audio := oggStub()
	body, ct, err := buildMultipart(groqForm{
		Model: "whisper-large-v3", Language: "tr", Prompt: "Ada, CLIProxyAPI, Çağrı", Kind: kindOgg, Audio: audio,
	}, "testboundary123")
	if err != nil {
		t.Fatal(err)
	}
	if ct != "multipart/form-data; boundary=testboundary123" {
		t.Fatalf("content type %q", ct)
	}
	fields, file, data := parseForm(t, body, ct)
	want := map[string]string{
		"model": "whisper-large-v3", "response_format": "verbose_json", "temperature": "0",
		"language": "tr", "prompt": "Ada, CLIProxyAPI, Çağrı",
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("field %s = %q want %q", k, fields[k], v)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("unexpected extra fields: %v", fields)
	}
	if file == nil || file.FileName() != "audio.ogg" || file.Header.Get("Content-Type") != "audio/ogg" {
		t.Fatalf("file part wrong: %+v", file)
	}
	if !bytes.Equal(data, audio) {
		t.Fatal("audio bytes altered")
	}
}

func TestBuildMultipartOmitsEmptyOptionalFields(t *testing.T) {
	body, ct, err := buildMultipart(groqForm{Model: "whisper-large-v3-turbo", Kind: kindWAV, Audio: sineWAV(0.1, 300)}, "")
	if err != nil {
		t.Fatal(err)
	}
	fields, file, _ := parseForm(t, body, ct)
	if _, ok := fields["language"]; ok {
		t.Error("language must be omitted for auto-detect")
	}
	if _, ok := fields["prompt"]; ok {
		t.Error("prompt must be omitted when empty")
	}
	if file.FileName() != "audio.wav" {
		t.Errorf("file name %q", file.FileName())
	}
}

func TestBuildMultipartUnknownKind(t *testing.T) {
	if _, _, err := buildMultipart(groqForm{Model: "m", Kind: kindUnknown, Audio: []byte{1}}, ""); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}
