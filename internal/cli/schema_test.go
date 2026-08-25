package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/testsupport"
)

// volatileMessages are check names whose message embeds a value read from the
// live host, so the golden pins the check's presence but not its wording.
var volatileMessages = map[string]bool{"disk-space": true}

// jsonCommands are the JSON-emitting read commands, each named for the schema
// it must satisfy. Every fixture in hwtest.Roots is crossed with all of them,
// so a new topology extends the contract without touching this table.
var jsonCommands = []struct {
	name string
	args []string
}{
	{"detect", []string{"detect", "--json"}},
	{"preflight", []string{"preflight", "--json"}},
}

// blindTo names the fixture/command pairs whose report is byte-identical to
// the reference host's, with the reason it is. A fixture varies something the
// command cannot see: detect reads hardware, so a stray modprobe.d file leaves
// it unmoved. That is a contract rather than an accident, and stating it here
// makes the assertion two-directional — a command that starts seeing a fixture
// fails, and so does one that quietly goes blind to it.
var blindTo = map[string]string{
	"detect-foreign-vfio": "detect reads hardware; modprobe.d is preflight's business",
	"preflight-bridge":    "no preflight check inspects PCIe bridges",
	"preflight-no-audio":  "preflight has no HDMI-audio-function check (a gap, not a decision)",
}

// TestJSONContract goldens each command's reference report in full and every
// other fixture as its departure from that reference. The fixtures are the
// reference host with one or two facts changed, so a full golden apiece buries
// those facts in a copy of the reference and rewrites all eleven whenever the
// shared part moves.
func TestJSONContract(t *testing.T) {
	baseline := make(map[string][]byte, len(jsonCommands))
	for _, c := range jsonCommands {
		baseline[c.name] = runJSON(t, c.name, c.args, "reference")
		testsupport.Golden(t, c.name+"-reference.json", baseline[c.name])
	}
	for _, c := range jsonCommands {
		for _, fx := range hwtest.RootNames() {
			if fx == "reference" {
				continue
			}
			name := c.name + "-" + fx
			t.Run(name, func(t *testing.T) {
				got := runJSON(t, c.name, c.args, fx)
				blind := testsupport.GoldenDelta(t, name+".delta", baseline[c.name], got)
				if reason, declared := blindTo[name]; blind != declared {
					if blind {
						t.Errorf("%s is now byte-identical to the reference report: "+
							"%s no longer sees anything this fixture varies", name, c.name)
					} else {
						t.Errorf("%s now differs from the reference report, so it is no "+
							"longer blind (%s); drop it from blindTo", name, reason)
					}
				}
			})
		}
	}
	t.Run("status-unapplied", func(t *testing.T) {
		testsupport.Golden(t, "status-unapplied.json",
			runJSON(t, "status", []string{"status", "--json"}, "reference"))
	})
}

// runJSON builds one fixture, runs one command against it, and returns the
// schema-validated, normalized report.
func runJSON(t *testing.T, schema string, args []string, fixture string) []byte {
	t.Helper()
	// Every required tool present, so platform.tools does not depend on
	// whatever the developer or runner happens to have installed.
	t.Setenv("PATH", hwtest.FakeTools(t, hw.RequiredTools...))
	root := t.TempDir()
	if err := fixtureBuilders[fixture](root); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	Run(append(slices.Clone(args), "--root", root), &out, &errBuf)
	if out.Len() == 0 {
		t.Fatalf("no JSON on stdout (stderr: %q)", errBuf.String())
	}
	validateSchema(t, schema, out.Bytes())
	return normalizeJSON(t, out.Bytes())
}

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
