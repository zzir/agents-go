package handler

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/docs"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// specOperations reads "METHOD /path" out of the generated OpenAPI document:
// its paths are keys at two spaces, their methods keys at four.
func specOperations(t *testing.T) []string {
	t.Helper()
	_, paths, ok := strings.Cut(string(docs.SpecYAML), "\npaths:\n")
	if !ok {
		t.Fatal("the OpenAPI document has no paths section")
	}
	pathKey := regexp.MustCompile(`^  (/\S*):$`)
	methodKey := regexp.MustCompile(`^    (get|post|put|patch|delete):$`)
	var ops []string
	path := ""
	for line := range strings.SplitSeq(paths, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			break // the next top-level section
		}
		if m := pathKey.FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if m := methodKey.FindStringSubmatch(line); m != nil {
			ops = append(ops, strings.ToUpper(m[1])+" "+path)
		}
	}
	slices.Sort(ops)
	return ops
}

// The route table and the published spec name the same operations: a route
// without its annotation, or an annotation whose route is gone, fails here.
func TestRoutesMatchTheOpenAPISpec(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Handlers{}.Register(engine.Group(server.APIPrefix))

	param := regexp.MustCompile(`[:*](\w+)`) // a catch-all is a parameter in the spec
	var routes []string
	for _, r := range engine.Routes() {
		path := param.ReplaceAllString(strings.TrimPrefix(r.Path, server.APIPrefix), "{$1}")
		routes = append(routes, r.Method+" "+path)
	}
	slices.Sort(routes)

	spec := specOperations(t)
	for _, r := range routes {
		if !slices.Contains(spec, r) {
			t.Errorf("route %s is not in the OpenAPI spec", r)
		}
	}
	for _, op := range spec {
		if !slices.Contains(routes, op) {
			t.Errorf("the OpenAPI spec documents %s, which no route serves", op)
		}
	}
}
