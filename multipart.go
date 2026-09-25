package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/textproto"
)

// groqForm is the multipart form sent to /audio/transcriptions.
type groqForm struct {
	Model    string
	Language string // omitted when empty (auto-detect)
	Prompt   string // omitted when empty
	Kind     audioKind
	Audio    []byte
}

// buildMultipart renders the form. temperature is always 0 and
// response_format is always verbose_json (segments carry no_speech_prob).
// boundary may be empty (random); tests pass a fixed one.
func buildMultipart(form groqForm, boundary string) ([]byte, string, error) {
	var buf bytes.Buffer
	buf.Grow(len(form.Audio) + len(form.Prompt) + 1024)
	w := multipart.NewWriter(&buf)
	if boundary != "" {
		if err := w.SetBoundary(boundary); err != nil {
			return nil, "", err
		}
	}
	fields := [][2]string{
		{"model", form.Model},
		{"response_format", "verbose_json"},
		{"temperature", "0"},
	}
	if form.Language != "" {
		fields = append(fields, [2]string{"language", form.Language})
	}
	if form.Prompt != "" {
		fields = append(fields, [2]string{"prompt", form.Prompt})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	name := form.Kind.filename()
	if name == "" {
		return nil, "", fmt.Errorf("no upload name for audio kind %d", form.Kind)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	header.Set("Content-Type", form.Kind.contentType())
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(form.Audio); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}
