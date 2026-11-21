// SPDX-License-Identifier: AGPL-3.0-or-later

package tx

import (
	"bufio"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kgbvax/auto7/internal/tx/web"
)

// The gate implements the controller of the Enable and Stop handlers.
var _ web.Controller = (*Gate)(nil)

// Import paths that the boundary rules name, relative to the module.
const (
	txDir   = "internal/tx"
	sendPkg = "internal/tx/send"
	pahoMod = "github.com/eclipse/paho.mqtt.golang"
)

// pahoAllowed lists the directories whose packages may import paho.
var pahoAllowed = []string{"internal/tx", "internal/bus", "cmd/auto7", "third_party", "test"}

// within reports whether the package path p is dir or below dir.
func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// boundaryViolations checks an import graph. imports maps each package
// directory, relative to the module root, to the import paths of its
// files. It gives one line per violation, sorted.
//
// Two rules apply. A package outside internal/tx must not import the
// senders. A package outside the allowed directories must not import
// the MQTT client, because only the bus client may publish.
func boundaryViolations(module string, imports map[string][]string) []string {
	var out []string
	for dir, list := range imports {
		for _, imp := range list {
			if imp == module+"/"+sendPkg && !within(dir, txDir) {
				out = append(out, dir+" imports "+imp+": only internal/tx may send to WSJT-X")
			}
			if within(imp, pahoMod) && !slices.ContainsFunc(pahoAllowed, func(a string) bool { return within(dir, a) }) {
				out = append(out, dir+" imports "+imp+": only the bus client may use MQTT")
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestBoundaryCheckerFindsViolations(t *testing.T) {
	const mod = "example.test/m"
	imports := map[string][]string{
		"internal/tx":       {mod + "/internal/tx/send", pahoMod},
		"internal/tx/web":   {mod + "/internal/tx/send"},
		"internal/bus":      {pahoMod},
		"cmd/auto7":         {pahoMod, mod + "/internal/tx"},
		"test/integration":  {pahoMod},
		"third_party/x":     {pahoMod + "/packets"},
		"internal/hunt":     {mod + "/internal/tx/send"},
		"internal/txt":      {mod + "/internal/tx/send"},
		"cmd/auto7/sub":     {mod + "/internal/tx/send"},
		"internal/ui":       {pahoMod + "/packets"},
		"internal/business": {pahoMod},
	}
	got := boundaryViolations(mod, imports)
	want := []string{
		"cmd/auto7/sub imports " + mod + "/internal/tx/send: only internal/tx may send to WSJT-X",
		"internal/business imports " + pahoMod + ": only the bus client may use MQTT",
		"internal/hunt imports " + mod + "/internal/tx/send: only internal/tx may send to WSJT-X",
		"internal/txt imports " + mod + "/internal/tx/send: only internal/tx may send to WSJT-X",
		"internal/ui imports " + pahoMod + "/packets: only the bus client may use MQTT",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("violations\n got %q\nwant %q", got, want)
	}
}

// moduleRoot finds the directory of go.mod above the test directory and
// reads the module path.
func moduleRoot(t *testing.T) (dir, module string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		f, err := os.Open(filepath.Join(dir, "go.mod"))
		if err == nil {
			defer f.Close()
			s := bufio.NewScanner(f)
			for s.Scan() {
				if m, ok := strings.CutPrefix(strings.TrimSpace(s.Text()), "module "); ok {
					return dir, strings.TrimSpace(m)
				}
			}
			t.Fatal("go.mod has no module line")
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod found")
		}
		dir = up
	}
}

// moduleImports parses the imports of every Go file of the module,
// test files and files behind build tags included. It skips hidden
// directories and testdata.
func moduleImports(t *testing.T, root string) map[string][]string {
	t.Helper()
	imports := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		dir := path.Clean(filepath.ToSlash(rel))
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			imports[dir] = append(imports[dir], imp)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return imports
}

// TestBoundary runs the rules on the whole module.
func TestBoundary(t *testing.T) {
	root, module := moduleRoot(t)
	imports := moduleImports(t, root)
	// The walk must see the senders and their user, else it reads the
	// wrong tree.
	if !slices.Contains(imports[txDir], module+"/"+sendPkg) {
		t.Fatal("the walk did not find the import of the senders in internal/tx")
	}
	if !slices.Contains(imports["internal/bus"], pahoMod) {
		t.Fatal("the walk did not find the MQTT import of internal/bus")
	}
	for _, v := range boundaryViolations(module, imports) {
		t.Error(v)
	}
}
