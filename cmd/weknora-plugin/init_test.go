package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginpkg "github.com/Tencent/WeKnora/internal/plugin"
)

// TestScaffoldGeneratesValidManifestAndCode scaffolds one plugin per
// extension type into a temp dir and checks the generated manifest passes
// host validation and the Go source references the right SDK registrar.
func TestScaffoldGeneratesValidManifestAndCode(t *testing.T) {
	for _, extType := range extensionTypes {
		t.Run(extType, func(t *testing.T) {
			outDir := filepath.Join(t.TempDir(), "plugin-"+extType)
			a := scaffoldArgs{
				Type:     extType,
				ID:       "com.example.test-plugin",
				OutDir:   outDir,
				Module:   "github.com/example/test-plugin",
				BinName:  "test-plugin",
				GRPCAddr: "127.0.0.1:50071",
			}
			if err := scaffold(a); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			for _, name := range []string{"plugin.yaml", "main.go", "go.mod", "README.md"} {
				if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
					t.Errorf("expected generated file %s: %v", name, err)
				}
			}
			manifestBytes, err := os.ReadFile(filepath.Join(outDir, "plugin.yaml"))
			if err != nil {
				t.Fatalf("read generated manifest: %v", err)
			}
			// The generated manifest must pass the same validation the host
			// applies at discovery time — this is the core lint contract.
			if _, err := pluginpkg.ParseManifest(manifestBytes); err != nil {
				t.Fatalf("generated manifest fails host validation: %v", err)
			}
			mainBytes, err := os.ReadFile(filepath.Join(outDir, "main.go"))
			if err != nil {
				t.Fatalf("read generated main.go: %v", err)
			}
			for _, want := range []string{
				"pluginsdk.ServeContext",
				`"com.example.test-plugin"`,
				registrarCall(extType),
			} {
				if !strings.Contains(string(mainBytes), want) {
					t.Errorf("main.go missing %q", want)
				}
			}
		})
	}
}

// TestGoModTemplateModes covers the two replace modes: published-module and
// local-checkout.
func TestGoModTemplateModes(t *testing.T) {
	a := scaffoldArgs{Module: "github.com/example/m", ID: "com.example.m", Type: "document_parser"}
	published := goModTemplate(a)
	if strings.Contains(published, "replace") {
		t.Errorf("published go.mod must not contain replace, got:\n%s", published)
	}
	local := goModTemplate(scaffoldArgs{Module: "m", ID: "i", Type: "datasource", SDKPath: `C:\repo with space\sdk\plugin`})
	if !strings.Contains(local, `replace github.com/Tencent/WeKnora/sdk/plugin => "C:/repo with space/sdk/plugin"`) {
		t.Errorf("local go.mod replace not quoted/slashed correctly:\n%s", local)
	}
}

// TestScaffoldRefusesOverwrite ensures init never clobbers an existing file.
func TestScaffoldRefusesOverwrite(t *testing.T) {
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "plugin.yaml"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := scaffold(scaffoldArgs{
		Type: "document_parser", ID: "com.example.dup", OutDir: outDir,
		Module: "m", BinName: "dup", GRPCAddr: "127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("expected overwrite refusal")
	}
}

func TestValidExtensionType(t *testing.T) {
	if validExtensionType("bogus") {
		t.Fatal("bogus type should be invalid")
	}
	for _, v := range extensionTypes {
		if !validExtensionType(v) {
			t.Errorf("type %q should be valid", v)
		}
	}
}

func TestHumanise(t *testing.T) {
	cases := map[string]string{
		"my-cool-parser": "My Cool Parser",
		"single":         "Single",
		"a-b-c":          "A B C",
	}
	for in, want := range cases {
		if got := humanise(in); got != want {
			t.Errorf("humanise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBasename(t *testing.T) {
	if got := basename("com.example.my-parser"); got != "my-parser" {
		t.Errorf("basename = %q, want my-parser", got)
	}
}
