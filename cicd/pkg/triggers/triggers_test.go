package triggers

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"gitea-cicd/pkg/config"
)

var cfg = &config.Config{
	Project: "gitea-ci-123456", Region: "us-east1", AppProject: "gitea-496920", AppZone: "us-east1-b",
	GiteaURL: "https://gitea.example", GiteaRepo: "JMR-dev/Gitea",
	PipelineRegions: []string{"us-east4", "us-central1", "us-west1"},
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
	scripts := map[string]string{"clone": CloneScript(cfg)}
	for _, m := range Modes {
		scripts["submit-"+m.Name] = SubmitScript(cfg, m)
	}
	for name, script := range scripts {
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
	for _, m := range Modes {
		s := SubmitScript(cfg, m)
		for _, want := range []string{
			"--project=gitea-ci-123456", "_APP_PROJECT=gitea-496920", "_DEPLOY_STATE_BUCKET=gitea-ci-123456-deploy-state",
			"serviceAccounts/$$sa@gitea-ci-123456.iam.gserviceaccount.com", "gs://gitea-ci-123456-build-source/source",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: submit script lacks %q", m.Name, want)
			}
		}
		for _, ph := range []string{"@CI@", "@SUFFIX@", "@APP@"} {
			if strings.Contains(s, ph) {
				t.Errorf("%s: placeholder %s left in script", m.Name, ph)
			}
		}
	}
}

// A preview runs a branch nobody has reviewed, so it must submit only as the
// preview accounts, and every other mode only as the deploy accounts.
func TestPreviewRunsOnlyAsPreviewAccounts(t *testing.T) {
	var previews int
	for _, m := range Modes {
		s := SubmitScript(cfg, m)
		preview := strings.Contains(s, `sa="cb-$$p-preview"`)
		deploy := strings.Contains(s, `sa="cb-$$p"`)
		switch {
		case m.Preview:
			previews++
			if !preview || deploy || m.Automatic || m.Approval || m.Name != "preview" {
				t.Errorf("%s: preview=%v deploy=%v automatic=%v approval=%v", m.Name, preview, deploy, m.Automatic, m.Approval)
			}
		case preview || !deploy:
			t.Errorf("%s submits as the preview accounts", m.Name)
		}
		if strings.Count(s, "--service-account=") != 1 {
			t.Errorf("%s: expected exactly one --service-account", m.Name)
		}
	}
	if previews != 1 {
		t.Fatalf("%d preview modes", previews)
	}
}

func TestOnlyGatedApprovesAndAllows(t *testing.T) {
	s := SubmitScript(cfg, Modes[0])
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

// Pipelines never run in the launchers' region: a launcher holds that region's
// build vCPUs while it waits, so a pipeline queued behind launchers there could
// wait forever. The app's region still reaches the pipeline as _REGION.
func TestPipelinesRunOutsideTheLaunchersRegion(t *testing.T) {
	for _, m := range Modes {
		s := SubmitScript(cfg, m)
		if !strings.Contains(s, `--region="$$region"`) || strings.Count(s, "gcloud builds submit /workspace/src") != 1 {
			t.Errorf("%s: the submit does not use the picked region", m.Name)
		}
		if strings.Contains(s, "--region=us-east1") {
			t.Errorf("%s: something still runs in the launchers' region", m.Name)
		}
		if !strings.Contains(s, "_REGION=us-east1") {
			t.Errorf("%s: the pipeline lost the app's region", m.Name)
		}
		if !strings.Contains(s, "printf '%s\\n' us-east4 us-central1 us-west1 | shuf") || strings.Contains(s, "@PIPELINE_REGIONS@") {
			t.Errorf("%s: pipeline regions not filled in", m.Name)
		}
	}
}

// pick_region takes the region with the fewest builds in flight, and a gcloud
// that fails (an account that may not list builds) still yields a region.
func TestPickRegion(t *testing.T) {
	script := unescape(SubmitScript(cfg, Modes[0]), nil)
	start := strings.Index(script, "pick_region() {")
	end := strings.Index(script[start:], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("no pick_region in the submit script")
	}
	fn := script[start : start+end+3]
	for name, tc := range map[string]struct {
		gcloud string
		want   []string
	}{
		// us-east4 has 2 builds in flight, us-west1 1, us-central1 none.
		"least loaded": {`case "$*" in *us-east4*) printf 'a\nb\n';; *us-west1*) echo a;; esac`, []string{"us-central1"}},
		"list fails":   {`exit 1`, cfg.PipelineRegions},
	} {
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(bin+"/gcloud", []byte("#!/bin/bash\n"+tc.gcloud+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+fn+"pick_region")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			got := strings.TrimSpace(string(out))
			if err != nil || !slices.Contains(tc.want, got) {
				t.Fatalf("pick_region = %q (err %v), want one of %v", got, err, tc.want)
			}
		})
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
