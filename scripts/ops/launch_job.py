#!/usr/bin/env python3
"""Run a gitea_ops.py command as a Kubernetes Job on the runner cluster's ops
pool, instead of on the machine that starts it.

The engine's automatic rollback lives in the same process as the release. On a
laptop, a closed lid or a dropped terminal kills both halfway, and Gitea can be
left stopped. As a Job (infra/pkg/ops) it keeps running when this launcher goes
away: the launcher only creates it and follows its log. Re-attach with
`launch_job.py logs`; nothing is lost by detaching.

It talks to the Kubernetes API directly, with a token from
`gcloud auth print-access-token` held in memory: no kubeconfig is written, and
neither kubectl nor gke-gcloud-auth-plugin is needed. The cluster's DNS
endpoint presents a publicly trusted certificate.

One Job at a time, by construction: the Job has a fixed name, so a second
launch while one runs is refused. A finished one is replaced.

Usage:
  launch_job.py [--project P --zone Z] run -- <gitea_ops.py arguments>
  launch_job.py logs        # follow the current Job's log
  launch_job.py status      # one line about the current Job
"""
import argparse
import datetime
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))


def engine_files():
    """The engine the Job runs, as paths under scripts/ops: gitea_ops.py, this
    launcher, and everything under ansible/ (its VM steps). The same set as
    release_gate.py's ENGINE; test_cicd.py checks."""
    out = ["gitea_ops.py", "launch_job.py"]
    for root, dirs, names in os.walk(os.path.join(HERE, "ansible")):
        dirs[:] = sorted(d for d in dirs if not d.startswith(".") and d != "__pycache__")
        out += sorted(os.path.relpath(os.path.join(root, n), HERE) for n in names
                      if not n.startswith(".") and not n.endswith((".pyc", ".swp", ".retry")))
    return out


def configmap_key(path):
    """A ConfigMap key cannot hold a /: ansible/steps/start.yml is
    ansible__steps__start.yml, and the volume's items put it back."""
    return path.replace("/", "__")


NAMESPACE = "gitea-ops"      # infra/pkg/ops.Namespace
KSA = "gitea-release"        # infra/pkg/ops.KSAName
APP = "gitea-ops"            # infra/pkg/ops.AppLabel
POOL_KEY, POOL = "gitea-runner-pool", "ops"   # infra/pkg/gke
JOB = "gitea-ops"
# Pinned by digest: the engine's whole toolchain (python3, gcloud, ssh,
# ssh-keygen, ssh-keyscan) comes from this one image.
IMAGE = ("gcr.io/google.com/cloudsdktool/google-cloud-cli:589.0.0-slim"
         "@sha256:159c2b19fee4d45a4ee0d5388f4f6a22d3598b217a26213e9fe3e0387e0d992c")
# Longer than a release with its soak and a data restore; shorter than the
# VM-side dead man's switch (the pause-timers step, 3h).
DEADLINE_SECONDS = 2 * 3600


def log(msg):
    print(f"--> {msg}", file=sys.stderr, flush=True)


class Kube:
    def __init__(self, project, zone, cluster):
        self.project, self.zone, self.cluster = project, zone, cluster
        ep = gcloud("container", "clusters", "describe", cluster, f"--zone={zone}", f"--project={project}",
                    "--format=value(controlPlaneEndpointsConfig.dnsEndpointConfig.endpoint)")
        if not ep:
            sys.exit(f"cluster {cluster} has no DNS endpoint")
        self.base = "https://" + ep.removeprefix("https://")
        self.token = None
        self.token_at = 0

    def _token(self):
        # Access tokens last an hour; a long log follow outlives one.
        if not self.token or time.time() - self.token_at > 1800:
            self.token = gcloud("auth", "print-access-token")
            self.token_at = time.time()
        return self.token

    def request(self, method, path, body=None, *, stream=False, ok404=False):
        req = urllib.request.Request(
            self.base + path, method=method,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Authorization": f"Bearer {self._token()}", "Content-Type": "application/json",
                     "Accept": "application/json"})
        try:
            r = urllib.request.urlopen(req, timeout=None if stream else 60)
        except urllib.error.HTTPError as e:
            if ok404 and e.code == 404:
                return None
            detail = e.read().decode(errors="replace")[:500]
            sys.exit(f"{method} {path}: HTTP {e.code}: {detail}")
        if stream:
            return r
        with r:
            return json.load(r)


def gcloud(*args):
    r = subprocess.run(["gcloud", *args], capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"gcloud {' '.join(args[:3])} failed: {r.stderr.strip()}")
    return r.stdout.strip()


def git(*args):
    r = subprocess.run(["git", "-C", HERE, *args], capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def job_path(name=JOB):
    return f"/apis/batch/v1/namespaces/{NAMESPACE}/jobs/{name}"


def job_state(job):
    """"active", "succeeded", "failed" or "pending" for a Job object."""
    st = job.get("status") or {}
    for c in st.get("conditions") or []:
        if c.get("status") == "True" and c.get("type") in ("Complete", "Failed"):
            return "succeeded" if c["type"] == "Complete" else "failed"
    return "active" if st.get("active") else "pending"


def manifests(engine_args, project):
    # A Cloud Build source tarball has no .git; the pipeline names the commit.
    commit = os.environ.get("COMMIT_SHA") or git("rev-parse", "HEAD")
    dirty = bool(git("status", "--porcelain", "--", "."))
    files = engine_files()
    data = {}
    for name in files:
        with open(os.path.join(HERE, name)) as f:
            data[configmap_key(name)] = f.read()
    labels = {"app": APP}
    annotations = {
        "gitea-ops/commit": commit + ("-dirty" if dirty else ""),
        "gitea-ops/args": " ".join(engine_args),
        "gitea-ops/started-by": gcloud("config", "get-value", "account") or "unknown",
        "gitea-ops/started-at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }
    configmap = {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": JOB, "namespace": NAMESPACE, "labels": labels, "annotations": annotations},
        "data": data,
    }
    job = {
        "apiVersion": "batch/v1", "kind": "Job",
        "metadata": {"name": JOB, "namespace": NAMESPACE, "labels": labels, "annotations": annotations},
        "spec": {
            # Never retried: a second attempt after a partial release must be
            # a person's decision, made from the manifest.
            "backoffLimit": 0,
            "activeDeadlineSeconds": DEADLINE_SECONDS,
            # Kept a week, so its log and status outlive the launcher.
            "ttlSecondsAfterFinished": 7 * 86400,
            "template": {
                "metadata": {
                    "labels": labels,
                    "annotations": {"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"},
                },
                "spec": {
                    "serviceAccountName": KSA,
                    "automountServiceAccountToken": False,
                    "restartPolicy": "Never",
                    "nodeSelector": {POOL_KEY: POOL, "iam.gke.io/gke-metadata-server-enabled": "true"},
                    "tolerations": [{"key": POOL_KEY, "operator": "Equal", "value": POOL, "effect": "NoSchedule"}],
                    "containers": [{
                        "name": "engine",
                        "image": IMAGE,
                        "command": ["python3", "-u", "/ops/gitea_ops.py", "--project", project, *engine_args],
                        "workingDir": "/work",
                        "env": [
                            # gcloud compute ssh writes its key under HOME.
                            {"name": "HOME", "value": "/work"},
                            {"name": "COMMIT_SHA", "value": commit},
                            {"name": "CLOUDSDK_CORE_DISABLE_PROMPTS", "value": "1"},
                            # The engine refuses --drill without it; only
                            # `make ci-drill` sets it, here and in the build.
                            *([{"name": "GITEA_DRILL_OK", "value": "1"}]
                              if os.environ.get("GITEA_DRILL_OK") == "1" else []),
                        ],
                        "resources": {"requests": {"cpu": "500m", "memory": "1Gi"},
                                      "limits": {"memory": "2Gi"}},
                        "volumeMounts": [{"name": "ops", "mountPath": "/ops", "readOnly": True},
                                         {"name": "work", "mountPath": "/work"}],
                    }],
                    "volumes": [
                        {"name": "ops", "configMap": {
                            "name": JOB, "defaultMode": 0o755,
                            "items": [{"key": configmap_key(n), "path": n} for n in files]}},
                        {"name": "work", "emptyDir": {}},
                    ],
                },
            },
        },
    }
    return configmap, job, dirty


def follow(kube):
    """Stream the Job pod's log until it ends; return the engine's exit code."""
    pod = None
    for _ in range(120):  # scaling the ops pool up from zero takes a few minutes
        pods = kube.request("GET", f"/api/v1/namespaces/{NAMESPACE}/pods?labelSelector=job-name%3D{JOB}")
        items = pods.get("items") or []
        if items:
            pod = items[-1]
            phase = pod["status"].get("phase")
            if phase in ("Running", "Succeeded", "Failed"):
                break
            reasons = [c.get("reason", "") for c in pod["status"].get("conditions") or [] if c.get("status") != "True"]
            log(f"pod {pod['metadata']['name']} is {phase} {'/'.join(r for r in reasons if r)}")
        else:
            log("waiting for the Job's pod")
        time.sleep(10)
    if not pod:
        sys.exit("no pod appeared for the Job")
    name = pod["metadata"]["name"]
    since = None
    while True:
        q = "follow=true&timestamps=true" + (f"&sinceTime={since}" if since else "")
        try:
            with kube.request("GET", f"/api/v1/namespaces/{NAMESPACE}/pods/{name}/log?{q}", stream=True) as r:
                for raw in r:
                    line = raw.decode(errors="replace").rstrip("\n")
                    ts, _, text = line.partition(" ")
                    if since and ts <= since:
                        continue
                    since = ts
                    print(text, flush=True)
        except (urllib.error.URLError, ConnectionError, TimeoutError) as e:
            log(f"log stream dropped ({e}); reconnecting -- the Job keeps running regardless")
            time.sleep(5)
            continue
        job = kube.request("GET", job_path())
        state = job_state(job)
        if state in ("succeeded", "failed"):
            break
        time.sleep(5)  # the stream ended but the Job has not; reconnect
    pod = kube.request("GET", f"/api/v1/namespaces/{NAMESPACE}/pods/{name}")
    for cs in pod["status"].get("containerStatuses") or []:
        term = (cs.get("state") or {}).get("terminated")
        if term:
            log(f"engine exited {term.get('exitCode')} ({term.get('reason', '')}); Job {state}")
            return int(term.get("exitCode", 1))
    log(f"Job {state}, no exit code found")
    return 0 if state == "succeeded" else 1


def cmd_run(kube, a):
    if not a.engine_args:
        sys.exit("nothing to run: pass gitea_ops.py arguments after --")
    existing = kube.request("GET", job_path(), ok404=True)
    if existing:
        state = job_state(existing)
        ann = existing["metadata"].get("annotations") or {}
        if state in ("active", "pending"):
            sys.exit(f"a Job is already running: {ann.get('gitea-ops/args')!r} since "
                     f"{ann.get('gitea-ops/started-at')}. Follow it with `launch_job.py logs`.")
        log(f"replacing the finished Job ({state}): {ann.get('gitea-ops/args')!r}")
        kube.request("DELETE", job_path(), {"propagationPolicy": "Background"})
        for _ in range(30):
            if not kube.request("GET", job_path(), ok404=True):
                break
            time.sleep(2)
        kube.request("DELETE", f"/api/v1/namespaces/{NAMESPACE}/configmaps/{JOB}", ok404=True)
    configmap, job, dirty = manifests(a.engine_args, kube.project)
    if dirty:
        log("WARNING: scripts/ops has uncommitted changes; the Job runs the working tree, "
            "and its commit annotation says -dirty")
    kube.request("POST", f"/api/v1/namespaces/{NAMESPACE}/configmaps", configmap)
    kube.request("POST", f"/apis/batch/v1/namespaces/{NAMESPACE}/jobs", job)
    log(f"Job {NAMESPACE}/{JOB} created: gitea_ops.py {' '.join(a.engine_args)}")
    if a.detach:
        log("detached; follow with `launch_job.py logs`")
        return 0
    return follow(kube)


def cmd_logs(kube, a):
    if not kube.request("GET", job_path(), ok404=True):
        sys.exit("no Job")
    return follow(kube)


def cmd_status(kube, a):
    job = kube.request("GET", job_path(), ok404=True)
    if not job:
        print("no Job")
        return 0
    ann = job["metadata"].get("annotations") or {}
    print(f"{job_state(job)}: gitea_ops.py {ann.get('gitea-ops/args')} "
          f"(commit {ann.get('gitea-ops/commit')}, started {ann.get('gitea-ops/started-at')} "
          f"by {ann.get('gitea-ops/started-by')})")
    return 0


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--project", default=os.environ.get("APP_PROJECT", "gitea-496920"))
    p.add_argument("--zone", default="us-east1-b")
    p.add_argument("--cluster", default="gitea-runners")
    sub = p.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run", help="create the Job and follow it")
    r.add_argument("--detach", action="store_true", help="create the Job and return")
    r.add_argument("engine_args", nargs=argparse.REMAINDER)
    sub.add_parser("logs", help="follow the current Job's log")
    sub.add_parser("status", help="show the current Job")
    a = p.parse_args()
    if a.cmd == "run" and a.engine_args[:1] == ["--"]:
        a.engine_args = a.engine_args[1:]
    kube = Kube(a.project, a.zone, a.cluster)
    sys.exit({"run": cmd_run, "logs": cmd_logs, "status": cmd_status}[a.cmd](kube, a))


if __name__ == "__main__":
    main()
