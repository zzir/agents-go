// Command verifydocs checks that the Go snippets in the documentation still
// name things that exist.
//
//	go run ./cmd/verifydocs        # check
//	go run ./cmd/verifydocs -v     # ...and list what was checked
//
// Why it exists: the docs are the API's other surface, and nothing compiled
// them. `go build ./...` covers the code and cmd/verifyexamples covers
// examples/, but a snippet in sessions.md is just text — it kept calling
// GetItems and PopItem for a while after both were renamed, and quickstart.md
// showed a guardrail type that had been merged away. The first thing a reader
// copies should not be the thing that fails to compile.
//
// It cannot compile the snippets: 114 of the 157 are fragments with no
// imports and undeclared variables, and wrapping each one would mean writing a
// preamble per snippet and keeping THAT correct. So it checks names instead,
// which catches the failure mode that actually happens — a symbol is renamed
// or removed and the prose keeps the old one.
//
// Two checks:
//
//   - A qualified reference (agents.Foo, tracing.Bar) whose package is one of
//     ours must name something that package exports.
//   - A method call whose receiver's type the snippet states — x from
//     `x := agents.NewFoo()` or `var x agents.Foo` — must name a method that
//     type has.
//
// The second check covers less than "every method call" but reports no false
// positives, which matters more: a check that cries wolf about errgroup.Go
// gets ignored, and then it is not a check. Receivers whose type the snippet
// never states are left alone.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// sdkPackages maps the import alias a snippet would use to the directory that
// defines it. A reference through one of these aliases is checked; anything
// else is assumed to be stdlib or a third-party package and left alone.
var sdkPackages = map[string]string{
	"agents":     "agents",
	"compaction": "agents/compaction",
	"middleware": "agents/middleware",
	"tasks":      "agents/tasks",
	"tracing":    "tracing",
	"sandbox":    "sandbox",
	"mcp":        "mcp",
	"memory":     "memory",
	"openai":     "models/openai",
	"sessions":   "sessions",
	"skills":     "skills",
	"docker":     "sandbox/docker",
	"ssh":        "sandbox/ssh",
}

var (
	goBlock     = regexp.MustCompile("(?s)```go\n(.*?)```")
	lineComment = regexp.MustCompile(`//.*`)
	stringLit   = regexp.MustCompile(`"[^"]*"`)
	qualified   = regexp.MustCompile(`\b([a-z][a-zA-Z0-9]*)\.([A-Z][A-Za-z0-9_]*)`)
	// x := agents.NewFoo(…)  |  x, err := …  |  x, y, err := …
	// Anchored at the line start and binding only the FIRST name: the others
	// are the constructor's later results, which are different types.
	ctorBind = regexp.MustCompile(`(?m)^\s*(\w+)(?:\s*,\s*\w+)*\s*:?=\s*([a-z][a-zA-Z0-9]*)\.([A-Z]\w*)\(`)
	// var x agents.Foo  |  var x *agents.Foo
	varBind    = regexp.MustCompile(`var\s+(\w+)\s+\*?([a-z][a-zA-Z0-9]*)\.([A-Z]\w*)`)
	methodCall = regexp.MustCompile(`\b(\w+)\.([A-Z][A-Za-z0-9_]*)\(`)
)

// pkgInfo is what one SDK package declares, indexed for the two checks.
type pkgInfo struct {
	exports map[string]bool            // every exported name
	methods map[string]map[string]bool // type name -> its method set
	ctors   map[string]string          // constructor name -> type it returns
}

func main() {
	verbose := flag.Bool("v", false, "list every file checked")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fail(err)
	}

	pkgs, err := loadSymbols(root)
	if err != nil {
		fail(err)
	}

	docs, err := docFiles(root)
	if err != nil {
		fail(err)
	}

	var problems []string
	blocks := 0
	for _, path := range docs {
		src, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range goBlock.FindAllStringSubmatch(string(src), -1) {
			blocks++
			problems = append(problems, checkBlock(rel, m[1], pkgs)...)
		}
		if *verbose {
			fmt.Printf("  %s\n", rel)
		}
	}

	fmt.Printf("verifydocs: %d Go blocks in %d files\n", blocks, len(docs))
	if len(problems) == 0 {
		fmt.Println("verifydocs: OK")
		return
	}
	sort.Strings(problems)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	fmt.Fprintf(os.Stderr, "\nverifydocs: %d problem(s)\n", len(problems))
	os.Exit(1)
}

// checkBlock reports every reference in one snippet that names nothing.
func checkBlock(file, block string, pkgs map[string]*pkgInfo) []string {
	// Comments and string literals are prose, not references.
	clean := stringLit.ReplaceAllString(lineComment.ReplaceAllString(block, ""), `""`)

	seen := map[string]bool{}
	var out []string
	report := func(msg string) {
		if !seen[msg] {
			seen[msg] = true
			out = append(out, fmt.Sprintf("%s: %s", file, msg))
		}
	}

	for _, m := range qualified.FindAllStringSubmatch(clean, -1) {
		pkg, ident := m[1], m[2]
		info, ours := pkgs[pkg]
		if !ours {
			continue
		}
		if !info.exports[ident] {
			report(fmt.Sprintf("%s.%s does not exist", pkg, ident))
		}
	}

	// Bind what the snippet states the type of, then check calls on those.
	bound := map[string][2]string{} // variable -> {package, type}
	for _, m := range ctorBind.FindAllStringSubmatch(clean, -1) {
		v, pkg, fn := m[1], m[2], m[3]
		if info, ours := pkgs[pkg]; ours {
			if typ, isCtor := info.ctors[fn]; isCtor {
				bound[v] = [2]string{pkg, typ}
			}
		}
	}
	for _, m := range varBind.FindAllStringSubmatch(clean, -1) {
		v, pkg, typ := m[1], m[2], m[3]
		if info, ours := pkgs[pkg]; ours {
			if info.methods[typ] != nil {
				bound[v] = [2]string{pkg, typ}
			}
		}
	}
	for _, m := range methodCall.FindAllStringSubmatch(clean, -1) {
		recv, name := m[1], m[2]
		b, known := bound[recv]
		if !known {
			continue // the snippet never says what this is
		}
		if !pkgs[b[0]].methods[b[1]][name] {
			report(fmt.Sprintf("%s.%s has no method %s (called on %s)", b[0], b[1], name, recv))
		}
	}
	return out
}

// loadSymbols parses the SDK's packages and returns what each one exports,
// plus the union of every exported method name across all of them.
func loadSymbols(root string) (map[string]*pkgInfo, error) {
	out := map[string]*pkgInfo{}

	for alias, dir := range sdkPackages {
		info := &pkgInfo{
			exports: map[string]bool{},
			methods: map[string]map[string]bool{},
			ctors:   map[string]string{},
		}
		files, err := goFiles(filepath.Join(root, dir))
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		for _, path := range files {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil, fmt.Errorf("parsing %s: %w", path, err)
			}
			collect(f, info)
		}
		if len(info.exports) == 0 {
			return nil, fmt.Errorf("package %s (%s) exported nothing — wrong path?", alias, dir)
		}
		out[alias] = info
	}
	return out, nil
}

// goFiles lists a directory's non-test Go sources. Build tags are ignored: a
// name behind one still exists as far as the docs are concerned.
func goFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// collect records one file's exported declarations into info.
func collect(f *ast.File, info *pkgInfo) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			if d.Recv != nil {
				if recv := receiverType(d); recv != "" {
					if info.methods[recv] == nil {
						info.methods[recv] = map[string]bool{}
					}
					info.methods[recv][d.Name.Name] = true
				}
				continue
			}
			info.exports[d.Name.Name] = true
			if typ := constructedType(d); typ != "" {
				info.ctors[d.Name.Name] = typ
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						info.exports[s.Name.Name] = true
						collectInterfaceMethods(s, info)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							info.exports[n.Name] = true
						}
					}
				}
			}
		}
	}
}

// receiverType returns the bare type name a method hangs off, dropping the
// pointer and any type parameters.
func receiverType(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	return typeName(d.Recv.List[0].Type)
}

// constructedType returns the type a function returns as its first result,
// which is what makes it usable as a constructor binding. Only same-package
// types count — a function returning someone else's type tells us nothing we
// can check.
func constructedType(d *ast.FuncDecl) string {
	if d.Type.Results == nil || len(d.Type.Results.List) == 0 {
		return ""
	}
	return typeName(d.Type.Results.List[0].Type)
}

// typeName reduces *Foo, Foo[T] and *Foo[T] to Foo, and reports "" for
// anything qualified by another package or otherwise not a plain local name.
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	case *ast.Ident:
		if ast.IsExported(t.Name) {
			return t.Name
		}
	}
	return ""
}

// collectInterfaceMethods records an interface's methods, which have no
// FuncDecl of their own but are just as callable.
func collectInterfaceMethods(s *ast.TypeSpec, info *pkgInfo) {
	iface, ok := s.Type.(*ast.InterfaceType)
	if !ok || iface.Methods == nil {
		return
	}
	for _, field := range iface.Methods.List {
		for _, n := range field.Names {
			if n.IsExported() {
				if info.methods[s.Name.Name] == nil {
					info.methods[s.Name.Name] = map[string]bool{}
				}
				info.methods[s.Name.Name][n.Name] = true
			}
		}
	}
}

// docFiles returns the Markdown that documents the SDK: docs/ plus the two
// READMEs. The private planning files are not part of the repo's docs.
func docFiles(root string) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			out = append(out, filepath.Join(root, "docs", e.Name()))
		}
	}
	out = append(out, filepath.Join(root, "README.md"))
	sort.Strings(out)
	return out, nil
}

// repoRoot walks up from the working directory to the module root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "docs")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no module root with a docs/ directory above %s", dir)
		}
		dir = parent
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "verifydocs:", err)
	os.Exit(1)
}
