package app_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// The OpenAPI document is what client work is built against, so it drifting
// from the router is not a documentation problem — it is a broken contract
// that nothing else would catch. This walks the real router and compares.
//
// The document may describe endpoints the router does not serve: §67 allows
// specifying ahead of implementation, and those are marked **planned**. The
// reverse is not allowed. An endpoint that exists and is undocumented is
// either an accident or a surface nobody reviewed.

// pathParam rewrites chi's {param} into the same shape OpenAPI uses, and
// normalises the parameter names so a router calling something `chatID` and a
// document calling it `chat_id` still match.
var pathParam = regexp.MustCompile(`\{[^}]+\}`)

func normalise(route string) string {
	route = strings.TrimSuffix(route, "/")
	if route == "" {
		route = "/"
	}
	return pathParam.ReplaceAllString(route, "{}")
}

// routerEndpoints walks the assembled router and returns every method and path
// it actually serves.
func routerEndpoints(t *testing.T, handler http.Handler) map[string]bool {
	t.Helper()

	router, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("the application handler is %T, which cannot be walked", handler)
	}

	found := make(map[string]bool)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		found[method+" "+normalise(route)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk the router: %v", err)
	}
	return found
}

func documentedEndpoints(t *testing.T) (live, planned map[string]bool, successCodes map[string][]string) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol", "rest", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	var document struct {
		Paths map[string]map[string]struct {
			Description string         `yaml:"description"`
			Summary     string         `yaml:"summary"`
			Responses   map[string]any `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	live = make(map[string]bool)
	planned = make(map[string]bool)
	successCodes = make(map[string][]string)
	for path, operations := range document.Paths {
		for method, operation := range operations {
			switch strings.ToUpper(method) {
			case http.MethodGet, http.MethodPost, http.MethodPut,
				http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
			default:
				continue // parameters, servers, and other non-operation keys
			}
			key := strings.ToUpper(method) + " " + normalise(path)
			if strings.Contains(operation.Description, "**planned**") ||
				strings.Contains(operation.Summary, "**planned**") {
				planned[key] = true
				continue
			}
			live[key] = true
			successCodes[key] = successStatuses(operation.Responses)
		}
	}
	return live, planned, successCodes
}

// successStatuses picks the 2xx codes an operation documents.
func successStatuses(responses map[string]any) []string {
	var codes []string
	for code := range responses {
		if strings.HasPrefix(code, "2") {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	return codes
}

// The server answers every request with the §66 envelope, including the ones
// that carry nothing back: httpx.NoContent writes 200 with an empty data
// object rather than a bodiless 204. Documenting 204 would tell a client to
// expect no body from a call that sends one, so it is wrong in a way no
// example would reveal — hence a test rather than a review note.
func TestNoOperationClaimsABodilessResponse(t *testing.T) {
	_, _, successCodes := documentedEndpoints(t)

	var offenders []string
	for endpoint, codes := range successCodes {
		for _, code := range codes {
			if code == "204" || code == "205" || code == "304" {
				offenders = append(offenders, endpoint+" documents "+code)
			}
		}
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d operations document a bodiless success, but every SOBH "+
			"response carries the envelope:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func TestEveryRouteIsDocumented(t *testing.T) {
	stack := newStack(t)

	served := routerEndpoints(t, stack.app.Handler())
	documented, planned, _ := documentedEndpoints(t)

	var undocumented []string
	for endpoint := range served {
		// chi registers these itself for unmatched requests; they are not part
		// of the API surface. /ws is the WebSocket upgrade, which is a
		// different protocol described in protocol/websocket — chi reports it
		// under every method because it is registered with Handle.
		if strings.HasPrefix(endpoint, "OPTIONS ") ||
			strings.Contains(endpoint, "/*") ||
			strings.HasSuffix(endpoint, " /ws") ||
			strings.HasPrefix(endpoint, "GET /metrics") ||
			strings.HasPrefix(endpoint, "GET /debug") {
			continue
		}
		if !documented[endpoint] && !planned[endpoint] {
			undocumented = append(undocumented, endpoint)
		}
	}

	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("%d endpoints are served but not in protocol/rest/openapi.yaml:\n  %s",
			len(undocumented), strings.Join(undocumented, "\n  "))
	}
}

// The other direction is a warning rather than a failure, because the document
// is deliberately allowed to run ahead of the implementation — but an endpoint
// that is documented as live and does not exist is a lie to every client
// author, so it fails.
func TestNoEndpointIsDocumentedAsLiveWithoutExisting(t *testing.T) {
	stack := newStack(t)

	served := routerEndpoints(t, stack.app.Handler())
	documented, _, _ := documentedEndpoints(t)

	var missing []string
	for endpoint := range documented {
		if !served[endpoint] {
			missing = append(missing, endpoint)
		}
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d endpoints are documented as live but are not served "+
			"(mark them **planned** or implement them):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}
