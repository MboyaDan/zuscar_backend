// Package archtest enforces the module boundary rules from docs/architecture.md section 3.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePrefix = "zuscar/backend/internal/"

// setupPackages are composition roots for operator commands, not domain modules. They may use
// repositories directly and are never imported by the API (see TestAPINeverImportsSetupOrSeedCode).
var setupPackages = map[string]bool{"provision": true, "devseed": true}

type pkgImports struct {
	file    string
	module  string // first path element under internal/
	imports []string
}

func collect(t *testing.T) []pkgImports {
	t.Helper()
	var out []pkgImports
	root := ".." // backend/internal
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		module := strings.Split(filepath.ToSlash(rel), "/")[0]
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		pi := pkgImports{file: filepath.ToSlash(rel), module: module}
		for _, im := range f.Imports {
			pi.imports = append(pi.imports, strings.Trim(im.Path.Value, `"`))
		}
		out = append(out, pi)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func internalModule(imp string) (module, rest string, ok bool) {
	if !strings.HasPrefix(imp, modulePrefix) {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(imp, modulePrefix), "/", 2)
	module = parts[0]
	if len(parts) > 1 {
		rest = parts[1]
	}
	return module, rest, true
}

func TestWalkerSeesPlatformPackages(t *testing.T) {
	n := 0
	for _, p := range collect(t) {
		if p.module == "platform" {
			n++
		}
	}
	if n < 5 {
		t.Fatalf("expected the platform packages to be scanned, found %d files", n)
	}
}

// Rule 3: internal/platform imports no domain module.
func TestPlatformImportsNoDomainModule(t *testing.T) {
	for _, p := range collect(t) {
		if p.module != "platform" {
			continue
		}
		for _, imp := range p.imports {
			if m, _, ok := internalModule(imp); ok && m != "platform" {
				t.Errorf("%s imports domain module %q", p.file, imp)
			}
		}
	}
}

// Rules 1, 2 and 5: modules talk through services only, and nothing imports reports.
func TestDomainModulesUseOtherModulesThroughServicesOnly(t *testing.T) {
	for _, p := range collect(t) {
		if p.module == "platform" || p.module == "archtest" || setupPackages[p.module] {
			continue
		}
		for _, imp := range p.imports {
			m, rest, ok := internalModule(imp)
			if !ok || m == "platform" || m == p.module {
				continue
			}
			if m == "reports" {
				t.Errorf("%s imports reports (reports reads via its own queries)", p.file)
			}
			first := strings.Split(rest, "/")[0]
			if first == "repository" || first == "handler" {
				t.Errorf("%s imports %q: other modules may use only service interfaces", p.file, imp)
			}
		}
	}
}

// Prompt section 8: no seed file may be imported by the API at startup. We check the real dependency graph.
func TestAPINeverImportsSetupOrSeedCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "zuscar/backend/cmd/api").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, banned := range []string{"/internal/devseed", "/internal/provision", "/internal/platform/migrate", "/internal/platform/testdb"} {
			if strings.HasSuffix(line, banned) {
				t.Errorf("cmd/api depends on %s: seed, bootstrap and migration code must never be part of the API", line)
			}
		}
	}
	if !strings.Contains(string(out), "zuscar/backend/internal/platform/http") {
		t.Fatal("sanity: expected the API graph to include platform/http")
	}
}

// Only cmd/seed-dev may import devseed.
func TestOnlySeedCommandImportsDevseed(t *testing.T) {
	for _, p := range collect(t) {
		for _, imp := range p.imports {
			if strings.HasSuffix(imp, "/internal/devseed") && p.module != "devseed" {
				t.Errorf("%s imports devseed", p.file)
			}
		}
	}
	err := filepath.WalkDir("../../cmd", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, im := range f.Imports {
			if strings.HasSuffix(strings.Trim(im.Path.Value, `"`), "/internal/devseed") && !strings.Contains(filepath.ToSlash(p), "cmd/seed-dev/") {
				t.Errorf("%s imports devseed; only cmd/seed-dev may", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
