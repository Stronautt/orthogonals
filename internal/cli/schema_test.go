package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

var update = flag.Bool("update", false, "rewrite golden files")

// volatileMessages are check names whose message embeds a value read from the
// live host, so the golden pins the check's presence but not its wording.
var volatileMessages = map[string]bool{"disk-space": true}

// contractCase pins one command's JSON output for one fixture.
type contractCase struct {
	name    string
	fixture string
	args    []string
	schema  string
}

// contractCases pairs every fixture in hwtest.Roots with both JSON-emitting
// read commands, so adding a topology to the registry extends the contract
// without touching this table.
func contractCases() []contractCase {
	var cases []contractCase
	for _, fx := range hwtest.RootNames() {
		cases = append(cases,
			contractCase{"detect-" + fx, fx, []string{"detect", "--json"}, "detect"},
			contractCase{"preflight-" + fx, fx, []string{"preflight", "--json"}, "preflight"},
		)
	}
	return append(cases,
		contractCase{"status-unapplied", "reference", []string{"status", "--json"}, "status"})
}

func TestJSONContract(t *testing.T) {
	for _, tt := range contractCases() {
		t.Run(tt.name, func(t *testing.T) {
			// Every required tool present, so platform.tools does not depend on
			// whatever the developer or runner happens to have installed.
			t.Setenv("PATH", hwtest.FakeTools(t, hw.RequiredTools...))
			root := t.TempDir()
			if err := fixtureBuilders[tt.fixture](root); err != nil {
				t.Fatal(err)
			}

			var out, errBuf bytes.Buffer
			Run(append(tt.args, "--root", root), &out, &errBuf)
			if out.Len() == 0 {
				t.Fatalf("no JSON on stdout (stderr: %q)", errBuf.String())
			}

			validateSchema(t, tt.schema, out.Bytes())
			checkGolden(t, tt.name+".json", normalizeJSON(t, out.Bytes()))
		})
	}
}

// validateSchema asserts the document satisfies the published contract.
func validateSchema(t *testing.T, name string, doc []byte) {
	t.Helper()
	path := filepath.Join("..", "..", "schema", name+".schema.json")
	compiler := jsonschema.NewCompiler()
	sch, err := compiler.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("output violates %s:\n%v", path, err)
	}
}

// normalizeJSON blanks the messages listed in volatileMessages so the golden
// stays stable across machines.
func normalizeJSON(t *testing.T, doc []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatal(err)
	}
	report, ok := v.(map[string]any)
	if !ok {
		return doc
	}
	checks, ok := report["checks"].([]any)
	if !ok {
		return doc
	}
	for _, c := range checks {
		check, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := check["name"].(string); volatileMessages[name] {
			check["message"] = "<host-dependent>"
			check["status"] = "<host-dependent>"
			delete(check, "remedy")
		}
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test ./internal/cli -update)", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the golden file:\n%s", name, diffLines(string(want), string(got)))
	}
}

// diffLines renders the first differing line with a little context.
func diffLines(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + wl + "\n  got:  " + gl
		}
	}
	return "(no line differs)"
}
