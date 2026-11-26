package imagedata

import (
	"encoding/base64"
	"testing"
)

func TestImageDecodersAcceptTheSameRasterFormats(t *testing.T) {
	for _, fixture := range []struct {
		mediaType string
		signature string
	}{
		{"image/png", "\x89PNG\r\n\x1a\n"},
		{"image/jpeg", "\xff\xd8\xff\xe0"},
		{"image/webp", "RIFF\x00\x00\x00\x00WEBPVP8 "},
	} {
		t.Run(fixture.mediaType, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString([]byte(fixture.signature))
			limit := len(fixture.signature)
			decoded, detected := Decode(encoded, limit)
			if string(decoded) != fixture.signature || detected != fixture.mediaType {
				t.Fatalf("detect media type = %q, data = %q", detected, decoded)
			}
			if got := DecodeAs(encoded, fixture.mediaType, limit); string(got) != fixture.signature {
				t.Fatalf("decode declared media type = %q", got)
			}
			if got := DecodeAs(encoded, fixture.mediaType, limit-1); got != nil {
				t.Fatal("accepted an image above the remaining byte budget")
			}
			if !MatchesType(decoded, fixture.mediaType, limit) || MatchesType(decoded, fixture.mediaType, limit-1) {
				t.Fatal("raw image type check did not enforce the same byte budget")
			}
		})
	}
}

func TestImageDecodersRejectInvalidOrMismatchedContent(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
	svg := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	if got := DecodeAs(png, "image/jpeg", 1024); got != nil {
		t.Fatal("accepted a PNG declared as JPEG")
	}
	if MatchesType([]byte("<svg></svg>"), "image/svg+xml", 1024) || MatchesType(nil, "", 1024) {
		t.Fatal("accepted unsupported raw image content")
	}
	for _, content := range []string{svg, png + "!"} {
		if data, mediaType := Decode(content, 1024); data != nil || mediaType != "" {
			t.Fatalf("detected an image for invalid or unsupported content: %q", mediaType)
		}
	}
}
