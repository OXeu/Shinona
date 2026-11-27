package httpserver

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func BenchmarkZstdTranscriptEncoding(b *testing.B) {
	var fixture bytes.Buffer
	for turn := range 30 {
		fmt.Fprintf(&fixture, "\x12\x80\x04Turn %d: review workspace changes\n", turn)
		for line := range 128 {
			fmt.Fprintf(&fixture, "ok workspace/component-%02d test case %03d in batch %d\n", line%7, line, turn)
		}
	}
	for _, variant := range []struct {
		name             string
		window           int
		lowMem, crc, all bool
	}{
		{"Stream1MiB", 1 << 20, false, true, false},
		{"Stream256KiB", 256 << 10, true, true, false},
		{"Stream256KiBNoCRC", 256 << 10, true, false, false},
		{"EncodeAll256KiBNoCRC", 256 << 10, true, false, true},
	} {
		b.Run(variant.name, func(b *testing.B) {
			writer, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1),
				zstd.WithWindowSize(variant.window), zstd.WithLowerEncoderMem(variant.lowMem), zstd.WithEncoderCRC(variant.crc))
			if err != nil {
				b.Fatal(err)
			}
			var buffer bytes.Buffer
			var encoded []byte
			encode := func() {
				if variant.all {
					encoded = writer.EncodeAll(fixture.Bytes(), encoded[:0])
					return
				}
				buffer.Reset()
				writer.Reset(&buffer)
				for chunk := range slices.Chunk(fixture.Bytes(), 32<<10) {
					if _, err := writer.Write(chunk); err != nil {
						b.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
			encode()
			for b.Loop() {
				encode()
			}
			b.ReportMetric(float64(max(len(encoded), buffer.Len())), "compressed-B/op")
		})
	}
}
