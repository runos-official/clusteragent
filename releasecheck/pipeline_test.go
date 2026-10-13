package releasecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHiddenRCPipelineDoesNotAdvertiseAndGuardsBeforeTag(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".foreman", "pipelines", "release-rc.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var pipeline struct {
		Advertises any `yaml:"advertises"`
		Steps      []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(raw, &pipeline); err != nil {
		t.Fatal(err)
	}
	if pipeline.Advertises != nil {
		t.Fatal("hidden RC pipeline advertises to the fleet")
	}
	if len(pipeline.Steps) < 2 {
		t.Fatal("missing release gates")
	}
	guard := pipeline.Steps[0].Run
	for _, version := range []string{"v1.2.3", "", "v1.2.3-rc.1"} {
		command := strings.ReplaceAll(guard, "${{ version }}", version)
		err := exec.Command("bash", "-c", command).Run()
		if (err == nil) != (version == "v1.2.3-rc.1") {
			t.Fatalf("guard result for %q: %v", version, err)
		}
	}
	if !strings.Contains(pipeline.Steps[1].Run, "scripts/release.sh") || !strings.Contains(pipeline.Steps[1].Run, "--check") {
		t.Fatal("RC pipeline must run canonical release gates before tag")
	}
}
