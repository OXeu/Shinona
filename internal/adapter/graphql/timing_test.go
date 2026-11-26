package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gql "github.com/graph-gophers/graphql-go"
)

type responseFixture struct{}

func (responseFixture) Text() string { return "<script>&\u2028" }
func (responseFixture) Failure() (*string, error) {
	return nil, errors.New("resolver failed")
}

func TestGraphQLResponseMatchesEncodedEnvelope(t *testing.T) {
	schema := gql.MustParseSchema("type Query { text: String!, failure: String }", &responseFixture{})
	handler := graphqlHTTP{schema: schema}
	for _, query := range []string{"{ text }", "{ text failure }", "{ unknownField }"} {
		t.Run(query, func(t *testing.T) {
			want, err := json.Marshal(schema.Exec(context.Background(), query, "", nil))
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]string{"query": query})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/graphql", bytes.NewReader(body)))
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" || !bytes.Equal(response.Body.Bytes(), want) {
				t.Fatalf("response = %d %q, want %q", response.Code, response.Body.Bytes(), want)
			}
		})
	}
}
