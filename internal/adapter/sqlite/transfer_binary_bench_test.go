//go:build linux

package sqlite

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/eclipse/paho.golang/packets"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/sqlite/transferpb"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	transferRPCPath   = "/shinona.transfer.v1.TransferService/ReadPage"
	mqttRequestTopic  = "page/request"
	mqttResponseTopic = "page/response"
)

func transferRPC(repo *Repository) http.Handler {
	return connect.NewServerStreamHandler(transferRPCPath, func(
		ctx context.Context, request *connect.Request[transferpb.PageRequest], stream *connect.ServerStream[transferpb.PageEvent],
	) error {
		records, err := loadTransferRecords(ctx, repo, protobufRequest(request.Msg))
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		return sendProtobufPage(records, stream.Send)
	})
}

func protobufRequest(request *transferpb.PageRequest) transferRequest {
	return transferRequest{Resource: request.GetResource(), Offset: int(request.GetOffset())}
}

func sendProtobufPage(records transferRecords, send func(*transferpb.PageEvent) error) error {
	meta := &transferpb.PageMetadata{}
	if records.nextOffset != nil {
		meta.NextOffset = new(int32(*records.nextOffset))
	}
	if records.transcript != nil {
		meta.Session = protobufSummary(records.transcript.Session)
		meta.SandboxSettings = &transferpb.SandboxSettings{
			Available: []string{string(sandbox.Fresh), string(sandbox.Shared)}, Selected: string(sandbox.Fresh),
		}
	} else {
		meta.Projects = make([]*transferpb.Project, 0, len(records.projects))
		for _, project := range records.projects {
			meta.Projects = append(meta.Projects, &transferpb.Project{Id: project.ID, Name: project.Name})
		}
	}
	if err := send(&transferpb.PageEvent{Item: &transferpb.PageEvent_Meta{Meta: meta}}); err != nil {
		return err
	}
	for _, session := range records.sessions {
		if err := send(&transferpb.PageEvent{Item: &transferpb.PageEvent_Session{Session: protobufSummary(session)}}); err != nil {
			return err
		}
	}
	if records.transcript != nil {
		for _, turn := range records.transcript.Turns {
			if err := send(&transferpb.PageEvent{Item: &transferpb.PageEvent_Turn{Turn: protobufTurn(turn)}}); err != nil {
				return err
			}
		}
	}
	return send(&transferpb.PageEvent{Item: &transferpb.PageEvent_Done{Done: true}})
}

func protobufSummary(session run.Session) *transferpb.SessionSummary {
	message := &transferpb.SessionSummary{
		Id: session.ID, Title: session.Title, DefaultTitle: session.TurnCount == 0 && !session.CustomTitle,
		TurnCount: int32(session.TurnCount), UpdatedAt: session.UpdatedAt.Format(time.RFC3339Nano),
		Pinned: session.Pinned, Active: session.Active, Unread: session.Unread,
	}
	if session.ProjectID != "" {
		message.ProjectId = new(session.ProjectID)
	}
	return message
}

func protobufTurn(turn run.Run) *transferpb.Turn {
	message := &transferpb.Turn{
		Id: turn.ID, Sequence: int32(turn.Sequence), Prompt: turn.Prompt, Status: string(turn.Status), Output: turn.Output,
		CreatedAt: turn.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: turn.UpdatedAt.Format(time.RFC3339Nano),
		Model: turn.Payload.Options.Model, ContextTokens: int32(turn.ContextTokens), ContextWindow: int32(turn.ContextWindow),
		Attachments: make([]string, 0, len(turn.Payload.Images)),
		Steering:    make([]*transferpb.SteeringMessage, 0, len(turn.Steering)),
		Parts:       make([]*transferpb.TurnPart, 0, len(turn.Parts)),
	}
	if turn.Policy != "" {
		message.Policy = new(string(turn.Policy))
	}
	if turn.StartedAt != nil {
		message.StartedAt = new(turn.StartedAt.Format(time.RFC3339Nano))
	}
	for _, image := range turn.Payload.Images {
		message.Attachments = append(message.Attachments, image.Name)
	}
	for _, step := range turn.Steering {
		message.Steering = append(message.Steering, &transferpb.SteeringMessage{Text: step.Text, Delivered: step.Delivered})
	}
	for _, part := range turn.Parts {
		message.Parts = append(message.Parts, &transferpb.TurnPart{
			Kind: part.Kind, Text: part.Text, Tool: part.Tool, Input: part.Input, Output: part.Output, Status: part.Status,
		})
	}
	return message
}

type transferMQTTInfo struct {
	URL  string `json:"url"`
	Hash string `json:"hash"`
}

func startTransferMQTT(t *testing.T, origin string, repo *Repository) transferMQTTInfo {
	t.Helper()
	certificate := transferCertificate(t)
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	tlsConfig := http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{certificate}})
	server := &webtransport.Server{
		H3:          &http3.Server{TLSConfig: tlsConfig, Handler: mux},
		CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == origin },
	}
	handle := func(load func(context.Context, []byte, func([]byte) error) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			session, err := server.Upgrade(w, r)
			if err != nil {
				http.Error(w, "WebTransport upgrade failed", http.StatusBadRequest)
				return
			}
			defer func() { _ = session.CloseWithError(0, "") }()
			stream, err := session.AcceptStream(session.Context())
			if err != nil {
				return
			}
			defer func() { _ = stream.Close() }()
			serveTransferMQTT(session.Context(), stream, load)
		}
	}
	mux.HandleFunc("/mqtt", handle(func(ctx context.Context, payload []byte, send func([]byte) error) error {
		var request transferpb.PageRequest
		if err := proto.Unmarshal(payload, &request); err != nil {
			return err
		}
		records, err := loadTransferRecords(ctx, repo, protobufRequest(&request))
		if err != nil {
			return err
		}
		return sendProtobufPage(records, func(event *transferpb.PageEvent) error {
			payload, err := proto.Marshal(event)
			if err != nil {
				return err
			}
			return send(payload)
		})
	}))
	mux.HandleFunc("/matrix", handle(func(ctx context.Context, payload []byte, send func([]byte) error) error {
		var request matrixRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return err
		}
		return streamMatrix(ctx, repo, request, send)
	}))
	done := make(chan error, 1)
	go func() { done <- server.Serve(udp) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = udp.Close()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Errorf("serve WebTransport benchmark: %v", err)
		}
	})
	hash := sha256.Sum256(certificate.Certificate[0])
	return transferMQTTInfo{URL: "https://" + udp.LocalAddr().String() + "/mqtt", Hash: base64.StdEncoding.EncodeToString(hash[:])}
}

func transferCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func serveTransferMQTT(ctx context.Context, stream io.ReadWriter, load func(context.Context, []byte, func([]byte) error) error) {
	input := bufio.NewReader(stream)

	first, err := readTransferMQTT(input)
	if err != nil || first.Type != packets.CONNECT {
		return
	}
	hello, ok := first.Content.(*packets.Connect)
	if !ok || hello.ProtocolVersion != 5 || hello.ProtocolName != "MQTT" || !hello.CleanStart {
		return
	}
	if _, err := (&packets.Connack{Properties: &packets.Properties{}}).WriteTo(stream); err != nil {
		return
	}
	second, err := readTransferMQTT(input)
	if err != nil || second.Type != packets.SUBSCRIBE {
		return
	}
	sub, ok := second.Content.(*packets.Subscribe)
	if !ok || sub.PacketID == 0 || len(sub.Subscriptions) != 1 || sub.Subscriptions[0].Topic != mqttResponseTopic ||
		sub.Subscriptions[0].QoS != 0 {
		return
	}
	if _, err := (&packets.Suback{PacketID: sub.PacketID, Reasons: []byte{0}, Properties: &packets.Properties{}}).WriteTo(stream); err != nil {
		return
	}

	output := bufio.NewWriterSize(stream, 32*1024)
	for {
		packet, err := readTransferMQTT(input)
		if err != nil || packet.Type != packets.PUBLISH {
			return
		}
		message, ok := packet.Content.(*packets.Publish)
		if !ok || message.Topic != mqttRequestTopic || message.QoS != 0 || message.Retain {
			return
		}
		err = load(ctx, message.Payload, func(payload []byte) error {
			reply := &packets.Publish{Topic: mqttResponseTopic, Payload: payload, Properties: &packets.Properties{}}
			if _, err := reply.WriteTo(output); err != nil {
				return err
			}
			return output.Flush()
		})
		if err != nil {
			return
		}
	}
}

func readTransferMQTT(input *bufio.Reader) (*packets.ControlPacket, error) {
	// Bound the MQTT remaining length before the packet decoder allocates its buffer.
	header, err := input.Peek(2)
	if err != nil {
		return nil, err
	}
	if header[1]&0x80 != 0 {
		header, err = input.Peek(3)
		if err != nil {
			return nil, err
		}
		if header[2]&0x80 != 0 || int(header[2])<<7|int(header[1]&0x7f) > 4096 {
			return nil, errors.New("MQTT benchmark request packet exceeds 4 KiB")
		}
	}
	return packets.ReadPacket(input)
}
