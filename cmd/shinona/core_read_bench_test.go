package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
)

func (a *benchmarkAPI) authenticatedBenchmarkRequest(b testing.TB, path string) *http.Request {
	b.Helper()
	endpoint, err := url.Parse(a.URL)
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, a.URL+path, nil)
	req.Header.Set("Origin", a.URL)
	req.Header.Set("Accept-Encoding", "zstd")
	for _, cookie := range a.Client().Jar.Cookies(endpoint) {
		req.AddCookie(cookie)
	}
	return req
}

func benchmarkRecorderResponse(b testing.TB, handler http.Handler, req *http.Request) benchmarkResponse {
	b.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req.Clone(req.Context()))
	response := recorder.Result()
	defer response.Body.Close()
	wire := recorder.Body.Bytes()
	body := wire
	if response.Header.Get("Content-Encoding") == "zstd" {
		decoder := benchmarkZstdDecoders.Get().(*zstd.Decoder)
		var err error
		body, err = decoder.DecodeAll(wire, nil)
		benchmarkZstdDecoders.Put(decoder)
		if err != nil {
			b.Fatal(err)
		}
	}
	if response.StatusCode != http.StatusOK {
		b.Fatalf("%s returned %d: %s", req.URL.Path, response.StatusCode, body)
	}
	return benchmarkResponse{body, response.Header, response.Trailer, len(wire)}
}

func benchmarkSessionsListFiltered(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	a.seedBenchmarkSessions(b, 256)
	response := a.rest(b, http.MethodGet, rest.Prefix+"/sessions", nil, http.StatusOK, nil)
	var initial benchmarkSessionList
	if err := json.Unmarshal(response.body, &initial); err != nil || len(initial.Projects) != 4 {
		b.Fatalf("get fixture projects: %v, %+v", err, initial.Projects)
	}
	requests := make([]*http.Request, len(initial.Projects))
	for i, project := range initial.Projects {
		requests[i] = a.authenticatedBenchmarkRequest(b, rest.Prefix+"/sessions?projectId="+url.QueryEscape(project.ID))
	}
	var size benchmarkPayloadSize
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		// A new router has an empty list snapshot history.
		handler := a.newHandler()
		b.StartTimer()
		index := i % len(requests)
		response := benchmarkRecorderResponse(b, handler, requests[index])
		var page struct{ Sessions []struct{ ProjectID string } }
		if err := json.Unmarshal(response.body, &page); err != nil || len(page.Sessions) != 30 {
			b.Fatalf("unexpected filtered list: %d sessions, %v", len(page.Sessions), err)
		}
		for _, session := range page.Sessions {
			if session.ProjectID != initial.Projects[index].ID {
				b.Fatalf("list returned a session from project %s", session.ProjectID)
			}
		}
		size.add(response)
	}
	size.report(b)
}

func benchmarkImage(b testing.TB, side int) (run.Image, []byte) {
	b.Helper()
	image := image.NewRGBA(image.Rect(0, 0, side, side))
	random := rand.NewChaCha8([32]byte{4})
	_, _ = random.Read(image.Pix)
	for i := 3; i < len(image.Pix); i += 4 {
		image.Pix[i] = 255
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image); err != nil {
		b.Fatal(err)
	}
	if data.Len() < 256<<10 || data.Len() > 1<<20 {
		b.Fatalf("fixture image size %d is outside 256 KiB–1 MiB", data.Len())
	}
	return run.Image{Name: "fixture.png", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(data.Bytes())}, data.Bytes()
}

func benchmarkTranscriptCold(b *testing.B, withImages bool) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	session := a.session(b, "")
	var image run.Image
	if withImages {
		image, _ = benchmarkImage(b, 350)
	}
	const turns = run.TranscriptPageSize
	for i := range turns {
		var payload run.Payload
		if withImages && i%5 == 0 {
			payload.Images = []run.Image{image}
		}
		a.completedTurnWithPayload(b, session, benchmarkPrompt, payload, run.Result{
			Parts: []run.TurnPart{{Kind: "tool", Tool: "test", Output: fmt.Sprintf("fixture log %d: %s", i, benchmarkPrompt)}},
		})
	}
	// An explicit cursor skips SQLite's latest-page cache. A fresh router
	// skips both REST caches, while leaving SQLite connections open.
	request := a.authenticatedBenchmarkRequest(b, benchmarkSessionPath(session)+"/transcript?beforeSequence="+strconv.Itoa(turns+1))
	verify := func(response benchmarkResponse) {
		b.Helper()
		var page sessionpb.TranscriptPage
		if err := proto.Unmarshal(response.body, &page); err != nil || !page.Complete || len(page.Turns) != turns {
			b.Fatalf("unexpected cold transcript: %d turns, complete=%t, %v", len(page.Turns), page.Complete, err)
		}
		for i, turn := range page.Turns {
			wantImage := withImages && i%5 == 0
			if (len(turn.Attachments) != 0) != wantImage || (wantImage && turn.Attachments[0] != image.Name) {
				b.Fatalf("turn %d has unexpected attachment metadata: %v", i, turn.Attachments)
			}
		}
	}
	first := benchmarkRecorderResponse(b, a.newHandler(), request)
	verify(first)
	phases := first.serverTiming()
	if !phases.has("transcript_json") || !phases.has("transcript_turn_encode") || first.header.Get("Content-Encoding") != "zstd" {
		b.Fatalf("cold read skipped SQLite, encoding or compression: %v, %v", first.header, first.trailer)
	}
	var size benchmarkPayloadSize
	for b.Loop() {
		b.StopTimer()
		handler := a.newHandler()
		b.StartTimer()
		response := benchmarkRecorderResponse(b, handler, request)
		verify(response)
		size.add(response)
	}
	size.report(b)
}

const benchmarkStreamTurns = 600

func (a *benchmarkAPI) seedBenchmarkStream(b testing.TB) string {
	b.Helper()
	session := a.session(b, "")
	random := rand.NewChaCha8([32]byte{1})
	log := make([]byte, 2048)
	for range benchmarkStreamTurns {
		_, _ = random.Read(log)
		a.completedTurn(b, session, benchmarkPrompt, run.Result{
			Parts: []run.TurnPart{{Kind: "tool", Tool: "test", Output: base64.StdEncoding.EncodeToString(log)}},
		})
	}
	return session
}

// Check the complete Protobuf response and actual stream phases once before
// measuring with a streaming client which does not retain the whole export.
func (a *benchmarkAPI) fullStreamBenchmarkClient(b testing.TB, client *http.Client, session string) benchmarkRead {
	b.Helper()
	verify := a.transcriptBenchmarkClient(b, client, session, benchmarkStreamTurns, false)
	probe, err := verify(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	if probe.wireBytes <= 512<<10 || probe.header.Get("Content-Encoding") != "zstd" ||
		!probe.serverTiming().has("transcript_load_batch") {
		b.Fatalf("fixture did not stream: %d bytes, %v, %v", probe.wireBytes, probe.header, probe.trailer)
	}
	path := a.URL + benchmarkSessionPath(session) + "/transcript?paged=false"
	return func(ctx context.Context) (benchmarkResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			return benchmarkResponse{}, err
		}
		request.Header.Set("Origin", a.URL)
		request.Header.Set("Accept-Encoding", "zstd")
		response, err := client.Do(request)
		if err != nil {
			return benchmarkResponse{}, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "zstd" ||
			response.Header.Get("Content-Type") != sessiontransfer.TranscriptExportMediaType {
			return benchmarkResponse{}, fmt.Errorf("unexpected full stream: %s, %v", response.Status, response.Header)
		}
		wire, err := io.Copy(io.Discard, response.Body)
		if err == nil && wire != int64(probe.wireBytes) {
			err = fmt.Errorf("full stream transferred %d bytes, want %d", wire, probe.wireBytes)
		}
		return benchmarkResponse{header: response.Header, trailer: response.Trailer, wireBytes: int(wire)}, err
	}
}

func benchmarkTranscriptFullStream(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	session := a.seedBenchmarkStream(b)
	request := a.fullStreamBenchmarkClient(b, a.Client(), session)
	monitor, err := startBenchmarkResources(a.databasePath)
	if err != nil {
		b.Fatal(err)
	}
	defer monitor.stop()
	var wireBytes int64
	for b.Loop() {
		response, err := request(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		wireBytes += int64(response.wireBytes)
	}
	metrics, err := monitor.finish(b.N)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(wireBytes)/float64(b.N), "wire-B/op")
	b.ReportMetric(metrics["peak-heap"].Value, "peak-heap-B")
	b.ReportMetric(metrics["peak-disk"].Value, "peak-disk-B")
}

func benchmarkAttachment(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	session := a.session(b, "")
	image, raw := benchmarkImage(b, 350)
	turn := a.completedTurnWithPayload(b, session, benchmarkPrompt, run.Payload{Images: []run.Image{image}}, run.Result{})
	request := a.authenticatedBenchmarkRequest(b, benchmarkSessionPath(session)+"/turns/"+turn.ID+"/attachments/0")
	for _, name := range []string{"Cold", "Hot"} {
		b.Run(name, func(b *testing.B) {
			handler := a.newHandler()
			read := func() benchmarkResponse {
				b.Helper()
				result := benchmarkRecorderResponse(b, handler, request)
				if !bytes.Equal(result.body, raw) || result.header.Get("Content-Type") != image.MediaType {
					b.Fatalf("invalid attachment: %d bytes, %v", len(result.body), result.header)
				}
				return result
			}
			if !read().serverTiming().has("attachment_load") || read().serverTiming().has("attachment_load") {
				b.Fatal("attachment fixture did not perform one cold read followed by a cache hit")
			}
			var size benchmarkPayloadSize
			for b.Loop() {
				if name == "Cold" {
					b.StopTimer()
					handler = a.newHandler()
					b.StartTimer()
				}
				size.add(read())
			}
			size.report(b)
		})
	}
}
