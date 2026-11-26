package graphql

import (
	"context"
	"encoding/json"
	"net/http"

	gql "github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/errors"
	"github.com/graph-gophers/graphql-go/introspection"
	"github.com/graph-gophers/graphql-go/trace/tracer"

	"github.com/OXeu/shinona/internal/timing"
)

type graphqlHTTP struct{ schema *gql.Schema }

type responseControllerKey struct{}

func (h graphqlHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), responseControllerKey{}, http.NewResponseController(w))
	var input struct {
		Query         string         `json:"query"`
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	decoded := timing.Start(ctx, "graphql_decode")
	err := json.NewDecoder(r.Body).Decode(&input)
	decoded()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	executed := timing.Start(ctx, "graphql_operation")
	response := h.schema.Exec(ctx, input.Query, input.OperationName, input.Variables)
	executed()
	encoded := timing.Start(ctx, "graphql_encode")
	data, err := json.Marshal(response)
	encoded()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

type timingTracer struct{}

func (timingTracer) TraceValidation(ctx context.Context) tracer.ValidationFinishFunc {
	finish := timing.Start(ctx, "graphql_validate")
	return func([]*errors.QueryError) { finish() }
}

func (timingTracer) TraceQuery(ctx context.Context, _, _ string, _ map[string]any, _ map[string]*introspection.Type) (context.Context, tracer.QueryFinishFunc) {
	finish := timing.Start(ctx, "graphql_execute")
	return ctx, func([]*errors.QueryError) { finish() }
}

func (timingTracer) TraceField(ctx context.Context, _, typeName, field string, _ bool, _ map[string]any) (context.Context, tracer.FieldFinishFunc) {
	if typeName != "Query" && typeName != "Mutation" {
		return ctx, func(*errors.QueryError) {}
	}
	finish := timing.Start(ctx, "graphql_field_"+field)
	return ctx, func(*errors.QueryError) { finish() }
}
