package releasecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The record model. `deployed` holds the last commit that was tagged, a candidate or a final. A
// final on a prod target also fast-forwards `main`, the prod record. The prod target holds
// deployed_branch: main, so a pipeline step reaches the record only through the placeholder, and
// the `main` literal lives in scripts/record-release.sh.

type releasePipeline struct {
	Steps []struct {
		Name   string   `yaml:"name"`
		Run    string   `yaml:"run"`
		Expect []string `yaml:"expect"`
	} `yaml:"steps"`
}

func readPipeline(t *testing.T, name string) (releasePipeline, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".foreman", "pipelines", name))
	if err != nil {
		t.Fatal(err)
	}
	var p releasePipeline
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p, string(raw)
}

func TestFinalPipelineChecksAndRecordsThroughTheScript(t *testing.T) {
	p, _ := readPipeline(t, "release.yml")
	var names []string
	for _, s := range p.Steps {
		names = append(names, s.Name)
	}
	want := []string{
		"Final comes from the validated candidate",
		"Gate chain",
		"Tag and push",
		"Wait for the release workflow",
		"Verify the published image attestation",
		"Record the release",
	}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("final pipeline steps are %q, want %q", names, want)
	}
	args := `"${{ version }}" "${{ target.env }}" "${{ target.branch }}" "${{ target.deployed_branch }}"`
	if got := p.Steps[0].Run; !strings.Contains(got, "scripts/record-release.sh check "+args) {
		t.Fatalf("the first step must run the record script in check mode with the target values, got:\n%s", got)
	}
	last := p.Steps[len(p.Steps)-1]
	if !strings.Contains(last.Run, "scripts/record-release.sh advance "+args) {
		t.Fatalf("the last step must run the record script in advance mode with the target values, got:\n%s", last.Run)
	}
	for _, fact := range []string{"main_before", "main_after", "main_equals_final"} {
		found := false
		for _, e := range last.Expect {
			found = found || e == fact
		}
		if !found {
			t.Fatalf("the record step must expect the fact %s, has %q", fact, last.Expect)
		}
	}
}

// A literal record branch in a step defeats the objective-scoped rewrite of
// ${{ target.deployed_branch }}. The retired branch may not appear anywhere in a pipeline.
func TestPipelinesNameNoRetiredAndNoLiteralRecordBranch(t *testing.T) {
	retired := "deployed/" + "prod"
	literal := regexp.MustCompile(`(^|[^\w-])(main|deployed/dev)([^\w/-]|$)`)
	for _, file := range []string{"release.yml", "release-rc.yml"} {
		p, raw := readPipeline(t, file)
		if strings.Contains(raw, retired) {
			t.Errorf("%s names the retired record branch %s", file, retired)
		}
		for _, s := range p.Steps {
			for _, line := range strings.Split(s.Run, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if literal.MatchString(line) {
					t.Errorf("%s step %q names a record branch literally: %s", file, s.Name, line)
				}
			}
		}
	}
}

// A candidate keeps advancing the record the target names, and nothing else.
func TestCandidatePipelineRecordsOnTheTargetBranch(t *testing.T) {
	p, _ := readPipeline(t, "release-rc.yml")
	last := p.Steps[len(p.Steps)-1]
	if last.Name != "Advance the deployed branch" || !strings.Contains(last.Run, `record="${{ target.deployed_branch }}"`) {
		t.Fatalf("the candidate pipeline must end by advancing ${{ target.deployed_branch }}, got step %q:\n%s", last.Name, last.Run)
	}
}

// The record script is tested against a local bare repository standing in for origin.
func TestRecordScript(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	out, err := exec.Command("bash", filepath.Join("..", "scripts", "record-release-test.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("record-release-test.sh failed: %v\n%s", err, out)
	}
}
