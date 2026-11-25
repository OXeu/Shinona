//go:build linux

package sqlite

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	"connectrpc.com/connect"
	"github.com/andybalholm/brotli"
	"github.com/fxamacker/cbor/v2"
	"github.com/gorilla/websocket"
	gql "github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/sqlite/transferpb"
	"github.com/OXeu/shinona/internal/run"
)

const (
	matrixPage = iota + 1
	matrixProject
	matrixSessionMeta
	matrixSession
	matrixTurn
	matrixDone
)

type matrixRequest struct {
	Resource    string
	Encoding    string
	Compression string
}

type matrixRecord struct {
	Kind    uint32   `json:"kind" msgpack:"kind" cbor:"kind"`
	Text    []string `json:"text" msgpack:"text" cbor:"text"`
	Numbers []int32  `json:"numbers" msgpack:"numbers" cbor:"numbers"`
}

func matrixSessionRecord(kind uint32, session run.Session) matrixRecord {
	return matrixRecord{
		Kind: kind,
		Text: []string{session.ID, session.ProjectID, session.Title, session.UpdatedAt.Format(time.RFC3339Nano)},
		Numbers: []int32{int32(session.TurnCount), number(session.TurnCount == 0 && !session.CustomTitle),
			number(session.Pinned), number(session.Active), number(session.Unread)},
	}
}

func matrixTurnRecord(turn run.Run) matrixRecord {
	started := ""
	if turn.StartedAt != nil {
		started = turn.StartedAt.Format(time.RFC3339Nano)
	}
	record := matrixRecord{
		Kind: matrixTurn,
		Text: []string{turn.ID, turn.Prompt, string(turn.Status), string(turn.Policy), turn.Output,
			turn.CreatedAt.Format(time.RFC3339Nano), started, turn.UpdatedAt.Format(time.RFC3339Nano), turn.Payload.Options.Model},
		Numbers: []int32{int32(turn.Sequence), int32(turn.ContextTokens), int32(turn.ContextWindow),
			int32(len(turn.Payload.Images)), int32(len(turn.Steering)), int32(len(turn.Parts))},
	}
	for _, image := range turn.Payload.Images {
		record.Text = append(record.Text, image.Name)
	}
	for _, message := range turn.Steering {
		record.Text = append(record.Text, message.Text)
		record.Numbers = append(record.Numbers, number(message.Delivered))
	}
	for _, part := range turn.Parts {
		record.Text = append(record.Text, part.Kind, part.Text, part.Tool, part.Input, part.Output, part.Status)
	}
	return record
}

func number(value bool) int32 {
	if value {
		return 1
	}
	return 0
}

func matrixPageRecords(records transferRecords) []matrixRecord {
	next := int32(-1)
	if records.nextOffset != nil {
		next = int32(*records.nextOffset)
	}
	rows := []matrixRecord{{Kind: matrixPage, Text: []string{}, Numbers: []int32{next}}}
	for _, project := range records.projects {
		rows = append(rows, matrixRecord{Kind: matrixProject, Text: []string{project.ID, project.Name}, Numbers: []int32{}})
	}
	for _, session := range records.sessions {
		rows = append(rows, matrixSessionRecord(matrixSession, session))
	}
	if records.transcript != nil {
		rows[0].Text = []string{"FRESH", "FRESH", "SHARED"} // selected policy, then available policies
		rows = append(rows, matrixSessionRecord(matrixSessionMeta, records.transcript.Session))
		for _, turn := range records.transcript.Turns {
			rows = append(rows, matrixTurnRecord(turn))
		}
	}
	return append(rows, matrixRecord{Kind: matrixDone, Text: []string{}, Numbers: []int32{}})
}

type matrixEncoder func([]matrixRecord) ([]byte, error)

var matrixEncoders = map[string]matrixEncoder{
	"json": func(records []matrixRecord) ([]byte, error) { return json.Marshal(records) },
	"ndjson": func(records []matrixRecord) ([]byte, error) {
		var data bytes.Buffer
		encoder := json.NewEncoder(&data)
		for _, row := range records {
			if err := encoder.Encode(row); err != nil {
				return nil, err
			}
		}
		return data.Bytes(), nil
	},
	"protobuf": func(records []matrixRecord) ([]byte, error) {
		batch := &transferpb.MatrixBatch{Records: make([]*transferpb.MatrixRecord, len(records))}
		for index, row := range records {
			batch.Records[index] = &transferpb.MatrixRecord{Kind: row.Kind, Text: row.Text, Numbers: row.Numbers}
		}
		return proto.Marshal(batch)
	},
	"messagepack": func(records []matrixRecord) ([]byte, error) { return msgpack.Marshal(records) },
	"bebop":       matrixBebop,
	"capnp":       matrixCapnp,
	"cbor":        func(records []matrixRecord) ([]byte, error) { return cbor.Marshal(records) },
}

// Bebop structs use little-endian fixed-width integers and uint32-prefixed arrays and UTF-8 strings.
// The matching schema is benchmarks/transfer/matrix.bop.
func matrixBebop(records []matrixRecord) ([]byte, error) {
	result := binary.LittleEndian.AppendUint32(nil, uint32(len(records)))
	for _, row := range records {
		result = binary.LittleEndian.AppendUint32(result, row.Kind)
		result = binary.LittleEndian.AppendUint32(result, uint32(len(row.Text)))
		for _, text := range row.Text {
			result = binary.LittleEndian.AppendUint32(result, uint32(len(text)))
			result = append(result, text...)
		}
		result = binary.LittleEndian.AppendUint32(result, uint32(len(row.Numbers)))
		for _, number := range row.Numbers {
			result = binary.LittleEndian.AppendUint32(result, uint32(number))
		}
	}
	return result, nil
}

// Use the field layout declared in benchmarks/transfer/matrix.capnp.
func matrixCapnp(records []matrixRecord) ([]byte, error) {
	message, segment := capnp.NewSingleSegmentMessage(nil)
	root, err := capnp.NewRootStruct(segment, capnp.ObjectSize{PointerCount: 1})
	if err != nil {
		return nil, err
	}
	rows, err := capnp.NewCompositeList(segment, capnp.ObjectSize{DataSize: 8, PointerCount: 2}, int32(len(records)))
	if err != nil {
		return nil, err
	}
	for index, row := range records {
		item := rows.Struct(index)
		item.SetUint32(0, row.Kind)
		text, err := capnp.NewTextList(segment, int32(len(row.Text)))
		if err != nil {
			return nil, err
		}
		for index, value := range row.Text {
			if err := text.Set(index, value); err != nil {
				return nil, err
			}
		}
		numbers, err := capnp.NewInt32List(segment, int32(len(row.Numbers)))
		if err != nil {
			return nil, err
		}
		for index, value := range row.Numbers {
			numbers.Set(index, value)
		}
		if err := item.SetPtr(0, text.ToPtr()); err != nil {
			return nil, err
		}
		if err := item.SetPtr(1, numbers.ToPtr()); err != nil {
			return nil, err
		}
	}
	if err := root.SetPtr(0, rows.ToPtr()); err != nil {
		return nil, err
	}
	return message.Marshal()
}

type matrixCompressor func([]byte) ([]byte, error)

func matrixCompression(name string) (matrixCompressor, func(), error) {
	closeWriter := func() {}
	switch name {
	case "raw":
		return func(data []byte) ([]byte, error) { return data, nil }, closeWriter, nil
	case "gzip":
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		return func(data []byte) ([]byte, error) {
			buffer.Reset()
			writer.Reset(&buffer)
			if _, err := writer.Write(data); err != nil {
				return nil, err
			}
			err := writer.Close()
			return buffer.Bytes(), err
		}, closeWriter, nil
	case "zstd":
		writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}
		return func(data []byte) ([]byte, error) { return writer.EncodeAll(data, nil), nil },
			func() { _ = writer.Close() }, nil
	case "brotli":
		var buffer bytes.Buffer
		writer := brotli.NewWriterLevel(&buffer, 4)
		return func(data []byte) ([]byte, error) {
			buffer.Reset()
			writer.Reset(&buffer)
			if _, err := writer.Write(data); err != nil {
				return nil, err
			}
			err := writer.Close()
			return buffer.Bytes(), err
		}, closeWriter, nil
	default:
		return nil, nil, fmt.Errorf("unknown compression %q", name)
	}
}

func streamMatrix(ctx context.Context, repo *Repository, request matrixRequest, send func([]byte) error) error {
	encode, ok := matrixEncoders[request.Encoding]
	if !ok {
		return fmt.Errorf("unknown encoding %q", request.Encoding)
	}
	compress, release, err := matrixCompression(request.Compression)
	if err != nil {
		return err
	}
	defer release()
	records, err := loadTransferRecords(ctx, repo, transferRequest{Resource: request.Resource})
	if err != nil {
		return err
	}
	for batch := range slices.Chunk(matrixPageRecords(records), streamBatchSize) {
		encoded, err := encode(batch)
		if err != nil {
			return err
		}
		payload, err := compress(encoded)
		if err != nil {
			return err
		}
		if err := send(payload); err != nil {
			return err
		}
	}
	return nil
}

type matrixResolver struct{ repo *Repository }

func (r matrixResolver) ReadMatrix(ctx context.Context, args matrixRequest) ([]string, error) {
	var frames []string
	err := streamMatrix(ctx, r.repo, args, func(payload []byte) error {
		frames = append(frames, base64.StdEncoding.EncodeToString(payload))
		return nil
	})
	return frames, err
}

func registerMatrix(mux *http.ServeMux, repo *Repository) {
	schema := gql.MustParseSchema(
		`type Query { readMatrix(resource: String!, encoding: String!, compression: String!): [String!]! }`,
		&matrixResolver{repo}, gql.MaxDepth(2),
	)
	mux.Handle("POST /matrix/graphql", &relay.Handler{Schema: schema})
	mux.HandleFunc("POST /matrix/http", matrixHTTP(repo, false))
	mux.HandleFunc("POST /matrix/sse", matrixHTTP(repo, true))
	mux.HandleFunc("GET /matrix/ws", matrixWS(repo))
	mux.Handle("/shinona.transfer.v1.TransferService/ReadMatrix", connect.NewServerStreamHandler(
		"/shinona.transfer.v1.TransferService/ReadMatrix",
		func(ctx context.Context, request *connect.Request[transferpb.MatrixRequest], stream *connect.ServerStream[transferpb.MatrixFrame]) error {
			params := matrixRequest{Resource: request.Msg.Resource, Encoding: request.Msg.Encoding, Compression: request.Msg.Compression}
			return streamMatrix(ctx, repo, params, func(payload []byte) error {
				return stream.Send(&transferpb.MatrixFrame{Payload: payload})
			})
		},
	))
}

func matrixHTTP(repo *Repository, sse bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request matrixRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
			http.Error(w, "invalid matrix request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		flusher := http.NewResponseController(w)
		err := streamMatrix(r.Context(), repo, request, func(payload []byte) error {
			if sse {
				if _, err := fmt.Fprintf(w, "data: %s\n\n", base64.StdEncoding.EncodeToString(payload)); err != nil {
					return err
				}
			} else {
				var size [4]byte
				binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
				if _, err := w.Write(size[:]); err != nil {
					return err
				}
				if _, err := w.Write(payload); err != nil {
					return err
				}
			}
			return flusher.Flush()
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			http.Error(w, "matrix stream failed", http.StatusBadRequest)
		}
	}
}

func matrixWS(repo *Repository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		conn.SetReadLimit(4096)
		for {
			var request matrixRequest
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			if err := streamMatrix(r.Context(), repo, request, func(payload []byte) error {
				return conn.WriteMessage(websocket.BinaryMessage, payload)
			}); err != nil {
				return
			}
		}
	}
}
