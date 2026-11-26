package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// DecodeJSON reads exactly one JSON value from a bounded request body.
func DecodeJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	return readJSON(jsonDecoder(w, r, limit), target)
}

// DecodeStrictJSON also rejects unknown object fields in the input type.
func DecodeStrictJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	decoder := jsonDecoder(w, r, limit)
	decoder.DisallowUnknownFields()
	return readJSON(decoder, target)
}

func jsonDecoder(w http.ResponseWriter, r *http.Request, limit int64) *json.Decoder {
	// Give net/http the original writer so it can close an oversized request's connection.
	for {
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
		}
		w = wrapper.Unwrap()
	}
}

func readJSON(decoder *json.Decoder, target any) error {
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	switch err := decoder.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return err
	default:
		return errors.New("multiple JSON values")
	}
}
