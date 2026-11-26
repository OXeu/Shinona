// Package imagedata validates the raster formats accepted in user uploads.
package imagedata

import (
	"encoding/base64"
	"net/http"
)

// Decode decodes an image from a bounded request and returns its detected type.
func Decode(encoded string, limit int) ([]byte, string) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ""
	}
	mediaType := rasterType(data, limit)
	if mediaType == "" {
		return nil, ""
	}
	return data, mediaType
}

// DecodeAs validates a declared media type and bounds the input before decoding.
func DecodeAs(encoded, mediaType string, limit int) []byte {
	if !supported(mediaType) || len(encoded) > base64.StdEncoding.EncodedLen(limit) {
		return nil
	}
	data, detected := Decode(encoded, limit)
	if detected != mediaType {
		return nil
	}
	return data
}

// MatchesType validates bounded image bytes against a declared raster type.
func MatchesType(data []byte, mediaType string, limit int) bool {
	return mediaType != "" && rasterType(data, limit) == mediaType
}

func rasterType(data []byte, limit int) string {
	if len(data) > limit {
		return ""
	}
	mediaType := http.DetectContentType(data)
	if !supported(mediaType) {
		return ""
	}
	return mediaType
}

func supported(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}
