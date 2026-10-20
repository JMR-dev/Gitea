package triggers

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gitea-cicd/pkg/config"
)

var cfg = &config.Config{
	Project: "gitea-ci-123456", Region: "us-east1", AppProject: "gitea-496920", AppZone: "us-east1-b",
	GiteaURL: "https://gitea.example", GiteaRepo: "JMR-dev/Gitea",
}

// unescape does what Cloud Build does to a step argument before the shell
// sees it: substitutions, then $$ -> $.
func unescape(s string, subs map[string]string) string {
	for k, v := range subs {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return strings.ReplaceAll(s, "$$", "$")
}

func TestScriptsParse(t *testing.T) {
	subs := map[string]string{"_SHA": "abc", "_PIPELINES": "infra,image", "_MODE": "deploy"}
	for name, script := range map[string]string{"clone": CloneScript(cfg), "submit": SubmitScript(cfg)} {
		t.Run(name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "*.sh")
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(unescape(script, subs))
			f.Close()
			if out, err := exec.Command("bash", "-n", f.Name()).CombinedOutput(); err != nil {
				t.Fatalf("bash -n: %v: %s", err, out)
			}
			// Every $ is either an escaped shell $ or one of our substitutions;
			// a stray one is a substitution Cloud Build would reject.
			rest := strings.ReplaceAll(script, "$$", "")
			for k := range subs {
				rest = strings.ReplaceAll(rest, "${"+k+"}", "")
			}
			if strings.Contains(rest, "$") {
				t.Fatalf("unescaped $ in %s script", name)
			}
		})
	}
}

func TestSubmitTargetsTheRightProjects(t *testing.T) {
	s := SubmitScript(cfg)
	for _, want := range []string{
		"--project=gitea-ci-123456", "_APP_PROJECT=gitea-496920", "_DEPLOY_STATE_BUCKET=gitea-ci-123456-deploy-state",
		"serviceAccounts/cb-$$p@gitea-ci-123456.iam.gserviceaccount.com", "gs://gitea-ci-123456-build-source/source",
		"--tags=\"gitea-pipeline,pipeline-$$p,sha-$$short\"",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("submit script lacks %q", want)
		}
	}
	if strings.Contains(s, "@") && strings.Contains(s, "@CI@") {
		t.Error("placeholder left in script")
	}
}

func TestOnlyGatedApprovesAndAllows(t *testing.T) {
	s := SubmitScript(cfg)
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "_APPROVED=true") && !strings.Contains(line, ":gated)") {
			t.Errorf("_APPROVED outside the gated mode: %s", line)
		}
	}
	var approval, ungated int
	for _, m := range Modes {
		if m.Approval {
			approval++
			if m.Name != "gated" {
				t.Errorf("%s needs approval", m.Name)
			}
		}
		if !m.Automatic {
			ungated++
			if m.Name != "preview" {
				t.Errorf("%s ignores autoDeploy", m.Name)
			}
		}
	}
	if approval != 1 || ungated != 1 {
		t.Fatalf("approval %d, ungated %d", approval, ungated)
	}
}

func TestCloneRefusesOffMainDeploys(t *testing.T) {
	s := CloneScript(cfg)
	if !strings.Contains(s, "deploy|gated|rebuild)") || !strings.Contains(s, "merge-base --is-ancestor") {
		t.Fatal("clone does not check deploys are on main")
	}
	if !strings.Contains(s, "https://gitea.example/JMR-dev/Gitea.git") {
		t.Fatal("clone URL")
	}
}
