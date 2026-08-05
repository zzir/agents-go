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
// Four checks:
//
//   - A qualified reference (agents.Foo, tracing.Bar) whose package is one of
//     ours must name something that package exports.
//   - A method call whose receiver's type the snippet states — x from
//     `x := agents.NewFoo()` or `var x agents.Foo` — must name a method that
//     type has.
//   - A [Symbol] doc link in a package's doc.go must name something that
//     package (or the qualified package) exports. The facade is the godoc a
//     reader sees first, and nothing else compiles it — it kept linking
//     [RunStreamed] and [InMemorySession] long after both were gone.
//   - A markdown link must land: a relative target names a file that exists,
//     and a #fragment — same-file or cross-file — names a heading in the file
//     it points into. Renaming a heading keeps every inbound link reading
//     fine as prose while it scrolls to nowhere.
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
	"slices"
	"strings"
)

// sdkPackages maps the import alias a snippet would use to the directory that
// defines it. A reference through one of these aliases is checked; anything
// else is assumed to be stdlib or a third-party package and left alone.
var sdkPackages = map[string]string{
	"agents":      "agents",
	"agentstest":  "agentstest",
	"compaction":  "agents/compaction",
	"middleware":  "agents/middleware",
	"session":     "agents/session",
	"tasks":       "agents/tasks",
	"tracing":     "tracing",
	"agentsotel":  "tracing/otel", // a bare otel. is the OpenTelemetry SDK's own package
	"sandbox":     "sandbox",
	"mcp":         "mcp",
	"memory":      "memory",
	"anthropic":   "models/anthropic",
	"modelkit":    "models/modelkit",
	"openai":      "models/openai",
	"sessions":    "sessions",
	"skills":      "skills",
	"bravesearch": "tools/bravesearch",
	"docker":      "sandbox/docker",
	"sshsb":       "sandbox/ssh", // a bare ssh. is golang.org/x/crypto/ssh
}

var (
	goBlock = regexp.MustCompile("(?s)```go\n(.*?)```")
	// A line comment or a string literal, matched in one alternation so that
	// whichever opens first swallows the other: a // inside a literal, and a
	// lone " inside a comment, each stay part of the construct already open.
	commentOrString = regexp.MustCompile(`"[^"]*"|//[^\n]*`)
	qualified       = regexp.MustCompile(`\b([a-z][a-zA-Z0-9]*)\.([A-Z][A-Za-z0-9_]*)`)
	// x := agents.NewFoo(…)  |  x, err := …  |  x, y, err := …
	// Anchored at the line start and binding only the FIRST name: the others
	// are the constructor's later results, which are different types.
	ctorBind = regexp.MustCompile(`(?m)^\s*(\w+)(?:\s*,\s*\w+)*\s*:?=\s*([a-z][a-zA-Z0-9]*)\.([A-Z]\w*)\(`)
	// var x agents.Foo  |  var x *agents.Foo
	varBind    = regexp.MustCompile(`var\s+(\w+)\s+\*?([a-z][a-zA-Z0-9]*)\.([A-Z]\w*)`)
	methodCall = regexp.MustCompile(`\b(\w+)\.([A-Z][A-Za-z0-9_]*)\(`)
	// A godoc doc link in a doc.go comment: [Name], [Type.Method] or
	// [pkg.Name]. The lowercase group is a package qualifier; only qualifiers
	// naming one of our packages are checked.
	docGoLink = regexp.MustCompile(`\[(?:([a-z][a-zA-Z0-9]*)\.)?([A-Z][A-Za-z0-9_]*)(?:\.([A-Z][A-Za-z0-9_]*))?\]`)
	// An inline markdown link target. Reference-style links are not used in
	// this repo's docs.
	mdLink  = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	heading = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*$`)
	// Code is stripped before link scanning: Go's `m[k](arg)` — fenced or
	// inline — reads as a markdown link otherwise, and neither renders as one.
	fence      = regexp.MustCompile("(?s)```.*?```")
	inlineCode = regexp.MustCompile("`[^`\n]+`")
)

// pkgInfo is what one SDK package declares, indexed for the two checks.
type pkgInfo struct {
	exports map[string]bool            // every exported name
	methods map[string]map[string]bool // type name -> its method set
	fields  map[string]map[string]bool // struct name -> its exported fields
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

	docProblems, links, derr := checkDocGo(root, pkgs)
	if derr != nil {
		fail(derr)
	}
	problems = append(problems, docProblems...)

	anchorProblems, mdLinks, aerr := checkAnchors(root, docs)
	if aerr != nil {
		fail(aerr)
	}
	problems = append(problems, anchorProblems...)

	fmt.Printf("verifydocs: %d Go blocks in %d files, %d doc.go links, %d markdown links\n",
		blocks, len(docs), links, mdLinks)
	if len(problems) == 0 {
		fmt.Println("verifydocs: OK")
		return
	}
	slices.Sort(problems)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	fmt.Fprintf(os.Stderr, "\nverifydocs: %d problem(s)\n", len(problems))
	os.Exit(1)
}

// checkBlock reports every reference in one snippet that names nothing.
func checkBlock(file, block string, pkgs map[string]*pkgInfo) []string {
	// Comments and string literals are prose, not references. A literal is
	// blanked rather than dropped so that what surrounds it stays separated.
	clean := commentOrString.ReplaceAllStringFunc(block, func(s string) string {
		if s[0] == '"' {
			return `""`
		}
		return ""
	})

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

// checkAnchors verifies that markdown links land: a relative target must name
// a file that exists, and a #fragment — same-file or cross-file — must name a
// heading in the file it points into. External URLs (a scheme) are left alone.
func checkAnchors(root string, files []string) (problems []string, count int, err error) {
	slugCache := map[string]map[string]bool{}
	load := func(path string) (map[string]bool, error) {
		if s, ok := slugCache[path]; ok {
			return s, nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		s := map[string]bool{}
		seen := map[string]int{}
		for line := range strings.SplitSeq(string(src), "\n") {
			m := heading.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			slug := slugify(m[1])
			// GitHub disambiguates repeated headings with -1, -2, …
			if n := seen[slug]; n > 0 {
				s[fmt.Sprintf("%s-%d", slug, n)] = true
			} else {
				s[slug] = true
			}
			seen[slug]++
		}
		slugCache[path] = s
		return s, nil
	}
	for _, path := range files {
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, 0, rerr
		}
		rel, _ := filepath.Rel(root, path)
		prose := inlineCode.ReplaceAllString(fence.ReplaceAllString(string(src), ""), "")
		for _, m := range mdLink.FindAllStringSubmatch(prose, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			count++
			file, frag, hasFrag := strings.Cut(target, "#")
			dest, label := path, "this file"
			if file != "" {
				label = file
				dest = filepath.Join(filepath.Dir(path), file)
				if _, serr := os.Stat(dest); serr != nil {
					problems = append(problems, fmt.Sprintf("%s: link target %s does not exist", rel, file))
					continue
				}
			}
			if !hasFrag || frag == "" || !strings.HasSuffix(dest, ".md") {
				continue // no fragment, or a fragment into non-markdown — not ours to judge
			}
			s, lerr := load(dest)
			if lerr != nil {
				return nil, 0, lerr
			}
			if !s[strings.ToLower(frag)] {
				problems = append(problems, fmt.Sprintf("%s: anchor #%s not found in %s", rel, frag, label))
			}
		}
	}
	return problems, count, nil
}

// slugify reduces a heading to its GitHub anchor: lowercase, punctuation
// dropped (underscores and hyphens survive), spaces to hyphens.
func slugify(h string) string {
	h = strings.ToLower(h)
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// checkDocGo verifies the [Symbol] doc links in each package's doc.go — the
// facade godoc renders first. Links to a package we do not index (stdlib,
// third-party) are left alone, like unqualified references in snippets.
func checkDocGo(root string, pkgs map[string]*pkgInfo) (problems []string, links int, err error) {
	aliases := make([]string, 0, len(sdkPackages))
	for a := range sdkPackages {
		aliases = append(aliases, a)
	}
	slices.Sort(aliases)
	for _, alias := range aliases {
		path := filepath.Join(root, sdkPackages[alias], "doc.go")
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue // not every package keeps a facade file
			}
			return nil, 0, rerr
		}
		rel, _ := filepath.Rel(root, path)
		// Comment lines only: a doc.go that ever grows example code must not
		// have its `Foo[T]` / `x[Index]` brackets read as doc links.
		var comments strings.Builder
		for line := range strings.SplitSeq(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				comments.WriteString(line)
				comments.WriteByte('\n')
			}
		}
		for _, m := range docGoLink.FindAllStringSubmatch(comments.String(), -1) {
			qual, name, method := m[1], m[2], m[3]
			info, label := pkgs[alias], name
			if qual != "" {
				other, ours := pkgs[qual]
				if !ours {
					continue
				}
				info, label = other, qual+"."+name
			}
			links++
			if method != "" {
				if !info.methods[name][method] && !info.fields[name][method] {
					problems = append(problems, fmt.Sprintf("%s: doc link [%s.%s] names no method or field of %s", rel, label, method, label))
				}
				continue
			}
			if !info.exports[name] {
				problems = append(problems, fmt.Sprintf("%s: doc link [%s] does not exist", rel, label))
			}
		}
	}
	return problems, links, nil
}

// loadSymbols parses the SDK's packages and returns what each one exports,
// plus the union of every exported method name across all of them.
func loadSymbols(root string) (map[string]*pkgInfo, error) {
	out := map[string]*pkgInfo{}

	for alias, dir := range sdkPackages {
		info := &pkgInfo{
			exports: map[string]bool{},
			methods: map[string]map[string]bool{},
			fields:  map[string]map[string]bool{},
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
	slices.Sort(out)
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
						collectStructFields(s, info)
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

// collectStructFields records a struct's exported field names: a godoc doc
// link may target [Type.Field] just as legally as [Type.Method], and a field
// reported as "no method" would teach authors to distrust the check.
func collectStructFields(s *ast.TypeSpec, info *pkgInfo) {
	st, ok := s.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return
	}
	for _, field := range st.Fields.List {
		for _, n := range field.Names {
			if n.IsExported() {
				if info.fields[s.Name.Name] == nil {
					info.fields[s.Name.Name] = map[string]bool{}
				}
				info.fields[s.Name.Name][n.Name] = true
			}
		}
	}
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
	out = append(out, filepath.Join(root, "cmd", "agents-server", "README.md"))
	slices.Sort(out)
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
