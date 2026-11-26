package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
)

// Count server WebSocket bytes below Gorilla's buffering and decompression.
// Exclude the upgrade response, even if it shares a read with a frame.
type benchmarkSocketMeter struct {
	net.Conn
	upgradeBytes int
	frameBytes   int64
}

func (m *benchmarkSocketMeter) Read(p []byte) (int, error) {
	n, err := m.Conn.Read(p)
	const end = "\r\n\r\n"
	for i := 0; i < n; i++ {
		if m.upgradeBytes == len(end) {
			m.frameBytes += int64(n - i)
			break
		}
		switch p[i] {
		case end[m.upgradeBytes]:
			m.upgradeBytes++
		case end[0]:
			m.upgradeBytes = 1
		default:
			m.upgradeBytes = 0
		}
	}
	return n, err
}

type benchmarkLiveSocket struct {
	*websocket.Conn
	meter *benchmarkSocketMeter
}

func (a *benchmarkAPI) openBenchmarkLive(b *testing.B, request []byte) benchmarkLiveSocket {
	b.Helper()
	meter := new(benchmarkSocketMeter)
	dialer := websocket.Dialer{
		EnableCompression: true,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			meter.Conn = conn
			return meter, nil
		},
	}
	conn := a.dialBenchmarkSocket(b, sessiontransfer.LivePath, dialer)
	benchmarkSocketWrite(b, conn, request)
	return benchmarkLiveSocket{conn, meter}
}

func benchmarkLiveRequest(b *testing.B, session string, known []*sessionpb.TurnVersion) []byte {
	b.Helper()
	body, err := proto.Marshal(&sessionpb.LiveRequest{SessionId: session, KnownTurns: known})
	if err != nil {
		b.Fatal(err)
	}
	return body
}

func (s benchmarkLiveSocket) next() (*sessionpb.LiveMessage, int, error) {
	if err := s.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, 0, err
	}
	for {
		kind, raw, err := s.ReadMessage()
		if err != nil {
			return nil, 0, err
		}
		msg := new(sessionpb.LiveMessage)
		if kind != websocket.BinaryMessage {
			return nil, 0, fmt.Errorf("unexpected live message type %d", kind)
		}
		if err := proto.Unmarshal(raw, msg); err != nil {
			return nil, 0, err
		}
		if !msg.GetHeartbeat() {
			return msg, len(raw), nil
		}
	}
}

func benchmarkLiveSync(b *testing.B) {
	a := newCoreBenchmarkAPI(b, demo.Agent{})
	session := a.seedBenchmarkTranscript(b, 120, 48)
	page, err := a.service.TranscriptPage(b.Context(), a.owner, session, 0)
	if err != nil || len(page.Turns) != run.TranscriptPageSize {
		b.Fatalf("read reconnect fixture: %v", err)
	}
	known := make([]*sessionpb.TurnVersion, 0, len(page.Turns)-1)
	for _, turn := range page.Turns[:len(page.Turns)-1] {
		known = append(known, &sessionpb.TurnVersion{
			Id: turn.ID, UpdatedAt: turn.UpdatedAt.Format(time.RFC3339Nano), Status: string(turn.Status),
		})
	}
	latest := page.Turns[len(page.Turns)-1].ID
	for _, mode := range []struct {
		name  string
		known []*sessionpb.TurnVersion
	}{
		{"Full", nil},
		{"Incremental", known},
	} {
		b.Run(mode.name, func(b *testing.B) {
			request := benchmarkLiveRequest(b, session, mode.known)
			var wire, body int64
			for b.Loop() {
				socket := a.openBenchmarkLive(b, request)
				msg, decoded, err := socket.next()
				wire += socket.meter.frameBytes
				body += int64(decoded)
				_ = socket.Close()
				if err != nil {
					b.Fatal(err)
				}
				sync := msg.GetSync()
				if msg.Sequence != 1 || len(sync.GetTurnIds()) != len(page.Turns) ||
					len(sync.GetChangedTurns()) != len(page.Turns)-len(mode.known) ||
					sync.ChangedTurns[len(sync.ChangedTurns)-1].Id != latest {
					b.Fatalf("unexpected %s reconnect: %v", mode.name, sync)
				}
			}
			b.ReportMetric(float64(wire)/float64(b.N), "wire-B/op")
			b.ReportMetric(float64(body)/float64(b.N), "body-B/op")
		})
	}
}

func benchmarkLiveStream(b *testing.B) {
	b.Run("Delta", func(b *testing.B) {
		for _, viewers := range []int{1, 8, 32} {
			b.Run(fmt.Sprintf("subscribers=%d", viewers), func(b *testing.B) {
				benchmarkLiveFanout(b, viewers)
			})
		}
	})
}

func benchmarkLiveFanout(b *testing.B, viewers int) {
	agent := benchmarkSteeringAgent{input: make(chan run.Input, 1)}
	a := newCoreBenchmarkAPI(b, agent)
	session := a.session(b, "")
	turn, err := a.service.Submit(b.Context(), a.owner, session, benchmarkPrompt, run.Payload{}, false)
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(b.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(a.repo, a.resolve, a.logger, a.controls, run.DefaultWorkerConcurrency).Serve(ctx)
	}()
	b.Cleanup(func() {
		cancel()
		<-done
	})
	var input run.Input
	select {
	case input = <-agent.input:
	case <-time.After(5 * time.Second):
		b.Fatal("agent did not start")
	}

	base := strings.Repeat("Initial agent output. ", 96)
	text, output := base, base
	report := func() {
		input.ReportParts([]run.TurnPart{
			{Kind: "text", Text: text},
			{Kind: "tool", Tool: "test", Output: output, Status: string(run.Running)},
		})
	}
	report()
	request := benchmarkLiveRequest(b, session, nil)
	sockets := make([]benchmarkLiveSocket, viewers)
	for i := range sockets {
		socket := a.openBenchmarkLive(b, request)
		b.Cleanup(func() { _ = socket.Close() })
		msg, _, err := socket.next()
		if err != nil || len(msg.GetSync().GetChangedTurns()) != 1 ||
			len(msg.GetSync().GetChangedTurns()[0].GetParts()) != 2 {
			b.Fatalf("subscriber %d did not receive the running turn: %v", i, err)
		}
		sockets[i] = socket
	}
	type result struct {
		msg        *sessionpb.LiveMessage
		wire, body int64
		err        error
	}
	results := make([]result, viewers)
	receive := func() {
		var readers sync.WaitGroup
		for i, socket := range sockets {
			readers.Go(func() {
				before := socket.meter.frameBytes
				msg, body, err := socket.next()
				results[i] = result{msg, socket.meter.frameBytes - before, int64(body), err}
			})
		}
		readers.Wait()
	}
	const textDelta, toolDelta = "Verified the result. ", "ok   workspace/component\n"
	var wire, body int64
	for i := 0; b.Loop(); i++ {
		if i%32 == 0 && i != 0 {
			b.StopTimer()
			text, output = base, base
			report()
			receive()
			for j, result := range results {
				parts := result.msg.GetProgress().GetParts()
				if result.err != nil || len(parts) != 2 || parts[0].GetReplace().GetText() != base {
					b.Fatalf("subscriber %d did not receive the reset: %v", j, result.err)
				}
			}
			b.StartTimer()
		}
		text += textDelta
		output += toolDelta
		report()
		receive()
		for j, result := range results {
			progress := result.msg.GetProgress()
			parts := progress.GetParts()
			if result.err != nil || progress.GetTurnId() != turn.ID || len(parts) != 2 ||
				parts[0].Index != 0 || parts[0].GetAppend().GetText() != textDelta ||
				parts[1].Index != 1 || parts[1].GetAppend().GetOutput() != toolDelta {
				b.Fatalf("subscriber %d did not receive the expected suffix delta: %v, %v", j, result.err, progress)
			}
			wire += result.wire
			body += result.body
		}
	}
	b.ReportMetric(float64(wire)/float64(b.N), "wire-B/op")
	b.ReportMetric(float64(wire)/float64(b.N)/float64(viewers), "wire-B/msg")
	b.ReportMetric(float64(body)/float64(b.N)/float64(viewers), "body-B/msg")
}
