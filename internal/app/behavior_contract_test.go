package app

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
)

func loadBehaviorContract(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	loader.Context = t.Context()
	contract, err := loader.LoadFromFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate(t.Context()); err != nil {
		t.Fatalf("invalid OpenAPI contract: %v", err)
	}
	return contract
}

// Validate the responses already exercised by behavior scenarios so schema
// changes and implementation changes cannot silently diverge.
func requireBehaviorContract(t *testing.T, contract *openapi3.T, prefix string, request *http.Request, response *http.Response, body []byte, payload map[string]any) {
	t.Helper()
	requestID := response.Header.Get("X-Request-ID")
	if requestID == "" {
		t.Fatal("HTTP response has no request correlation")
	}
	if response.StatusCode >= 400 {
		if payload["request_id"] != requestID {
			t.Fatal("error body and header have different request IDs")
		}
		if err := contract.Components.Schemas["ErrorResponse"].Value.VisitJSON(payload); err != nil {
			t.Fatalf("error response violates the OpenAPI contract: %v", err)
		}
	}
	if prefix != "" && !strings.HasPrefix(request.URL.Path, prefix+"/") {
		return
	}
	operationPath := strings.TrimPrefix(request.URL.Path, prefix)
	path := contract.Paths.Value(operationPath)
	if path == nil {
		return
	}
	operation := path.GetOperation(request.Method)
	if operation == nil {
		return
	}
	input := &openapi3filter.RequestValidationInput{
		Request: request,
		Route: &routers.Route{
			Spec: contract, Path: operationPath, PathItem: path,
			Method: request.Method, Operation: operation,
		},
		// The service's authentication is exercised by rejection scenarios;
		// this validator checks the request and response shapes.
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if response.StatusCode < 300 {
		original := request.Clone(t.Context())
		if request.GetBody != nil {
			var err error
			original.Body, err = request.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = original.Body.Close() }()
		}
		input.Request = original
		if err := openapi3filter.ValidateRequest(t.Context(), input); err != nil {
			t.Fatalf("accepted request violates the OpenAPI contract: %v", err)
		}
	}
	if err := openapi3filter.ValidateResponse(t.Context(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input, Status: response.StatusCode, Header: response.Header,
		Body: io.NopCloser(bytes.NewReader(body)), Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}); err != nil {
		t.Fatalf("HTTP response violates the OpenAPI contract: %v", err)
	}
}
