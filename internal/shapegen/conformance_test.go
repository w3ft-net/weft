package shapegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The hand-written half of package shape names no column, so it must work over
// any shape the generator can emit, not just the hosted one. For each shape this
// builds a scratch module out of the hand-written files, the generated tuple and
// a generated property test, and runs it: the algebra and the generated
// operations agree with each other for every column set.
func TestConformance_AlgebraIsShapeIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and tests a module per shape")
	}
	for name, yaml := range map[string]string{"hosted": hostedYAML, "default": defaultYAML, "odd": oddYAML, "namespace only": "coordinates: [namespace]\n"} {
		t.Run(name, func(t *testing.T) {
			spec, err := ParseSpec([]byte(yaml))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			write := func(file string, b []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", []byte("module shapetest\n\ngo 1.25\n"))
			for _, f := range []string{"shape.go", "layout.go"} {
				b, err := os.ReadFile(filepath.Join("..", "..", "shape", f))
				if err != nil {
					t.Fatal(err)
				}
				write(f, b)
			}
			src, err := Generate(spec, name+".yaml")
			if err != nil {
				t.Fatal(err)
			}
			write("coord_gen.go", src)
			props, err := GenerateConformanceTest(spec)
			if err != nil {
				t.Fatal(err)
			}
			write("conformance_gen_test.go", props)

			cmd := exec.Command("go", "test", "-count=1", "./...")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the %s shape's generated code fails its conformance tests: %v\n%s", name, err, out)
			}
		})
	}
}
