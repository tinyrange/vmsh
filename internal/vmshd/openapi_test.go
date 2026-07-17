package vmshd

import (
	"context"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"j5.nz/cc/client"
)

func TestOpenAPIContract(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "vmshd.openapi.yaml")
	loader := openapi3.NewLoader()
	document, err := loader.LoadFromFile(path)
	if err != nil {
		t.Fatalf("load OpenAPI document: %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate OpenAPI document: %v", err)
	}

	for _, contract := range []struct {
		name  string
		value any
	}{
		{name: "ErrorResponse", value: client.ErrorResponse{}},
		{name: "Status", value: Status{}},
		{name: "Event", value: Event{}},
		{name: "FrontendSummary", value: FrontendSummary{}},
		{name: "RegisterFrontendRequest", value: RegisterFrontendRequest{}},
		{name: "CreateSessionRequest", value: CreateSessionRequest{}},
		{name: "UpdateSessionRequest", value: UpdateSessionRequest{}},
		{name: "PersistSessionRequest", value: PersistSessionRequest{}},
		{name: "MCPEndpointInfo", value: MCPEndpointInfo{}},
		{name: "MCPCredential", value: MCPCredential{}},
		{name: "Session", value: Session{}},
		{name: "SessionSummary", value: SessionSummary{}},
		{name: "AttachSessionRequest", value: AttachSessionRequest{}},
		{name: "AttachSessionResponse", value: AttachSessionResponse{}},
		{name: "DetachSessionRequest", value: DetachSessionRequest{}},
		{name: "Terminal", value: Terminal{}},
		{name: "TerminalStreamMessage", value: TerminalStreamMessage{}},
		{name: "StartHostJobRequest", value: StartHostJobRequest{}},
		{name: "JobSummary", value: JobSummary{}},
	} {
		assertSchemaFields(t, document, contract.name, reflect.TypeOf(contract.value))
	}

	server := NewServer("test-token")
	server.RegisterHandlers(http.NewServeMux(), nil)
	registered := make(map[string]struct{})
	for _, route := range server.apiRoutes() {
		key := route.Method + " " + normalizeRoutePath(route.Path)
		if _, duplicate := registered[key]; duplicate {
			t.Fatalf("duplicate registered vmshd route %s", key)
		}
		registered[key] = struct{}{}
	}
	documented := make(map[string]struct{})
	for path, item := range document.Paths.Map() {
		for method := range item.Operations() {
			documented[strings.ToUpper(method)+" "+normalizeRoutePath(path)] = struct{}{}
		}
	}
	if missing, extra := routeDifference(registered, documented), routeDifference(documented, registered); len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("OpenAPI route drift: missing=%v extra=%v", missing, extra)
	}

	events := document.Paths.Find("/vmsh/events").Get.Responses.Value("200").Value.Content["application/x-ndjson"]
	if events == nil || events.Schema == nil || events.Schema.Ref != "#/components/schemas/Event" {
		t.Fatalf("event stream schema = %#v, want Event NDJSON", events)
	}
	stream := document.Paths.Find("/vmsh/sessions/{session_id}/attachments/{attachment_id}/stream").Get
	assertWebSocketExtension(t, stream.Extensions, "clientMessages", "#/components/schemas/TerminalStreamMessage")
	assertWebSocketExtension(t, stream.Extensions, "serverMessages", "#/components/schemas/TerminalStreamMessage")
}

func assertSchemaFields(t *testing.T, document *openapi3.T, name string, typ reflect.Type) {
	t.Helper()
	ref, ok := document.Components.Schemas[name]
	if !ok {
		t.Fatalf("OpenAPI schema %s is missing", name)
	}
	documented := make(map[string]struct{})
	collectSchemaFields(ref, documented)
	actual := make(map[string]struct{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" {
			name = field.Name
		}
		if name != "-" {
			actual[name] = struct{}{}
		}
	}
	if missing, extra := routeDifference(actual, documented), routeDifference(documented, actual); len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("OpenAPI schema %s field drift: missing=%v extra=%v", name, missing, extra)
	}
}

func collectSchemaFields(ref *openapi3.SchemaRef, fields map[string]struct{}) {
	if ref == nil || ref.Value == nil {
		return
	}
	for name := range ref.Value.Properties {
		fields[name] = struct{}{}
	}
	for _, part := range ref.Value.AllOf {
		collectSchemaFields(part, fields)
	}
}

func normalizeRoutePath(path string) string {
	var normalized strings.Builder
	for {
		start := strings.IndexByte(path, '{')
		if start < 0 {
			normalized.WriteString(path)
			return normalized.String()
		}
		end := strings.IndexByte(path[start:], '}')
		if end < 0 {
			normalized.WriteString(path)
			return normalized.String()
		}
		normalized.WriteString(path[:start])
		normalized.WriteString("{}")
		path = path[start+end+1:]
	}
}

func routeDifference(left, right map[string]struct{}) []string {
	var difference []string
	for route := range left {
		if _, ok := right[route]; !ok {
			difference = append(difference, route)
		}
	}
	sort.Strings(difference)
	return difference
}

func assertWebSocketExtension(t *testing.T, extensions map[string]any, field, want string) {
	t.Helper()
	websocket, ok := extensions["x-websocket"].(map[string]any)
	if !ok {
		t.Fatal("x-websocket extension is missing")
	}
	schema, ok := websocket[field].(map[string]any)
	if !ok {
		t.Fatalf("x-websocket.%s is missing", field)
	}
	if got, _ := schema["$ref"].(string); got != want {
		t.Fatalf("x-websocket.%s ref = %q, want %q", field, got, want)
	}
}
