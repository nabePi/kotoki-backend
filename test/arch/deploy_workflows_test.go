package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const (
	testSHA    = "0123456789abcdef0123456789abcdef01234567"
	testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{testutil.ModuleRoot(t)}, rel...)...))
	require.NoError(t, err)
	return string(b)
}

func workflowFiles() []string {
	return []string{"ci.yml", "deploy-staging.yml", "deploy-prod.yml"}
}

// jobs splits a workflow into its jobs (name -> text); keys of the `jobs:` map are indented two spaces.
func jobs(t *testing.T, wf string) map[string]string {
	t.Helper()
	_, body, ok := strings.Cut(wf, "\njobs:\n")
	require.True(t, ok, "workflow has jobs")
	re := regexp.MustCompile(`(?m)^  ([a-z0-9-]+):\s*$`)
	idx := re.FindAllStringSubmatchIndex(body, -1)
	require.NotEmpty(t, idx)
	out := map[string]string{}
	for i, m := range idx {
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out[body[m[2]:m[3]]] = body[m[0]:end]
	}
	return out
}

// permissionScopes returns every `scope: level` granted by any `permissions:` block, flow or block style.
func permissionScopes(wf string) map[string]string {
	got := map[string]string{}
	lines := strings.Split(wf, "\n")
	pair := regexp.MustCompile(`([a-z-]+):\s*(read|write|none)`)
	for i, l := range lines {
		m := regexp.MustCompile(`^(\s*)permissions:\s*(.*)$`).FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if rest := strings.TrimSpace(m[2]); rest != "" {
			for _, p := range pair.FindAllStringSubmatch(rest, -1) {
				got[p[1]] = p[2]
			}
			if !strings.HasPrefix(rest, "{") { // read-all / write-all
				got["*"] = rest
			}
			continue
		}
		for _, n := range lines[i+1:] {
			if strings.TrimSpace(n) == "" {
				continue
			}
			if len(n)-len(strings.TrimLeft(n, " ")) <= len(m[1]) {
				break
			}
			for _, p := range pair.FindAllStringSubmatch(n, -1) {
				got[p[1]] = p[2]
			}
		}
	}
	return got
}

func TestDeployWorkflowsShape(t *testing.T) {
	staging := readRepoFile(t, ".github", "workflows", "deploy-staging.yml")
	prod := readRepoFile(t, ".github", "workflows", "deploy-prod.yml")

	t.Run("staging", func(t *testing.T) {
		require.Regexp(t, `(?m)^name: deploy-staging$`, staging)
		require.Regexp(t, `(?m)^  workflow_run:\n    workflows: \[ci\]\n    types: \[completed\]\n    branches: \[main\]$`, staging)
		require.Contains(t, staging, "github.event.workflow_run.conclusion == 'success'")
		require.Contains(t, staging, "github.event.workflow_run.head_branch == 'main'")
		require.Contains(t, staging, "ref: ${{ github.event.workflow_run.head_sha }}")
		require.Contains(t, staging, "deploy/k8s/hack/deploy.sh staging")
		require.Contains(t, staging, "make k8s-tools cli", "smoke needs bin/kotoki-cli")
		require.Contains(t, staging, "secrets.STAGING_SMOKE_EMAIL")
		require.NotRegexp(t, `(?m)^\s+tags:`, staging)
		require.NotRegexp(t, `(?m)^\s+pull_request`, staging)
	})

	t.Run("prod", func(t *testing.T) {
		require.Regexp(t, `(?m)^name: deploy-prod$`, prod)
		require.Regexp(t, `(?m)^  push:\n    tags: \["v\*"\]$`, prod)
		require.Regexp(t, `(?m)^  workflow_dispatch:\n    inputs:\n      tag:\n`, prod)
		require.Contains(t, prod, "deploy/k8s/hack/deploy.sh prod")
		require.Contains(t, prod, "uses: actions/download-artifact@")
		require.Contains(t, prod, "pattern: digests-*")
		require.NotRegexp(t, `(?m)^\s+branches:`, prod)
		require.NotRegexp(t, `(?m)^\s+pull_request`, prod)
	})

	for _, name := range workflowFiles() {
		wf := readRepoFile(t, ".github", "workflows", name)
		t.Run(name+"/hygiene", func(t *testing.T) {
			require.NotRegexp(t, `:latest\b`, wf)

			pinned := regexp.MustCompile(`@[0-9a-f]{40}(\s|$)`)
			uses := regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)`)
			ms := uses.FindAllStringSubmatch(wf, -1)
			require.NotEmpty(t, ms)
			for _, m := range ms {
				require.Regexp(t, pinned, m[1]+"\n", "action %s must be pinned to a commit SHA", m[1])
			}

			head, _, ok := strings.Cut(wf, "\njobs:")
			require.True(t, ok)
			require.Regexp(t, `(?m)^permissions:\n  contents: read$`, head, "workflow default must be contents: read")

			// Minimal grants: contents:read everywhere; packages:write only for ci build-images; actions:read for
			// artifact download in the deploy workflows; id-token never (no OIDC federation is configured).
			allowed := map[string]string{"contents": "read"}
			if name == "ci.yml" {
				allowed["packages"] = "write"
			} else {
				allowed["actions"] = "read"
			}
			for scope, level := range permissionScopes(wf) {
				require.Equal(t, allowed[scope], level, "permission %s: %s not allowed in %s", scope, level, name)
			}
			require.NotContains(t, wf, "pull_request_target")
		})
		t.Run(name+"/no-secrets", func(t *testing.T) {
			for _, bad := range []string{`ghp_[A-Za-z0-9]{20,}`, `github_pat_[A-Za-z0-9_]{20,}`, `AKIA[0-9A-Z]{16}`,
				`-----BEGIN [A-Z ]*PRIVATE KEY`, `eyJ[A-Za-z0-9_-]{20,}\.`, `(?i)postgres(ql)?://[^\s$]*:[^\s$@]+@`} {
				require.NotRegexp(t, bad, wf, bad)
			}
			// Any credential-looking key must take its value from the secrets context (or be empty).
			cred := regexp.MustCompile(`(?mi)^\s*(?:-\s+)?([a-z0-9_-]*(?:password|passwd|token|secret|api[_-]?key|kubeconfig)[a-z0-9_-]*):\s*(.*)$`)
			for _, m := range cred.FindAllStringSubmatch(wf, -1) {
				if m[1] == "id-token" {
					continue
				}
				val := strings.Trim(strings.TrimSpace(m[2]), `"'`)
				if val == "" || strings.HasPrefix(val, "|") || strings.HasPrefix(val, ">") {
					continue
				}
				require.Regexp(t, `^\$\{\{\s*(secrets\.[A-Z0-9_]+|github\.token|steps\.[a-z0-9_.-]+|env\.[A-Z0-9_]+)\s*\}\}$`, val,
					"%s must reference the secrets context, not a literal", m[1])
			}
		})
	}
}

// Every job that deploys (calls deploy.sh or kubectl) must run under a protected GitHub Environment;
// production's reviewers are repository settings (documented as MG-1), the binding to it lives here.
func TestDeployJobsUseEnvironments(t *testing.T) {
	for file, env := range map[string]string{"deploy-staging.yml": "staging", "deploy-prod.yml": "production"} {
		wf := readRepoFile(t, ".github", "workflows", file)
		deploying := 0
		for name, body := range jobs(t, wf) {
			if !strings.Contains(body, "deploy.sh") && !strings.Contains(body, "kubectl ") && !strings.Contains(body, "KUBECONFIG") {
				continue
			}
			deploying++
			require.Regexp(t, `(?m)^    environment: `+env+`$`, body, "%s job %s deploys without environment %s", file, name, env)
			require.Regexp(t, `(?m)^    concurrency:`, body, "%s job %s must serialise deploys", file, name)
		}
		require.Equal(t, 1, deploying, "%s: exactly one deploying job", file)
	}
	prod := readRepoFile(t, ".github", "workflows", "deploy-prod.yml")
	require.NotContains(t, prod, "environment: staging")
	require.NotRegexp(t, `environment:\s*\{`, prod, "environment must be a plain name")
}

func TestDeployScriptStatic(t *testing.T) {
	s := readRepoFile(t, "deploy", "k8s", "hack", "deploy.sh")
	require.Contains(t, s, "set -euo pipefail")
	iMigrate := strings.Index(s, "# STAGE migrate")
	iSeed := strings.Index(s, "# STAGE seed")
	iApply := strings.Index(s, "# STAGE apply")
	iRollout := strings.Index(s, "# STAGE rollout")
	require.True(t, 0 < iMigrate && iMigrate < iSeed && iSeed < iApply && iApply < iRollout, "stage order migrate < seed < apply < rollout")
	require.Contains(t, s, "rollout status")
	require.Contains(t, s, "condition=complete job/kotoki-migrate")
	require.NotRegexp(t, `:latest\b`, s)
	c := readRepoFile(t, "deploy", "k8s", "hack", "secrets-checksum.sh")
	require.Contains(t, c, "set -euo pipefail")
	require.Contains(t, c, "kotoki.io/secrets-checksum")
}

// deployFixture writes stub kubectl, kustomize and kotoki-cli that log their arguments (one line per call,
// all into one log, so ordering across tools is observable). STUB_FAIL_ON makes a call whose arguments contain
// the text exit 1.
func deployFixture(t *testing.T) (env []string, logFile string) {
	t.Helper()
	tmp := t.TempDir()
	logFile = filepath.Join(tmp, "calls.log")
	stub := func(name, extra string) string {
		p := filepath.Join(tmp, name)
		body := "#!/bin/sh\necho \"" + name + " $*\" >> \"$CALL_LOG\"\n" +
			"case \"$*\" in *\"${STUB_FAIL_ON:-@@none@@}\"*) exit 1;; esac\n" + extra + "exit 0\n"
		require.NoError(t, os.WriteFile(p, []byte(body), 0o755))
		return p
	}
	kubectl := stub("kubectl", "case \"$*\" in *\"get secret\"*) echo \"B=${STUB_SECRET_V:-v1}\"; echo \"A=1\";; esac\n")
	kustomize := stub("kustomize", "case \"$1\" in build) printf 'kind: ConfigMap\\n';; esac\n")
	cli := stub("kotoki-cli", "")
	env = []string{"PATH=" + os.Getenv("PATH"), "CALL_LOG=" + logFile, "KUBECTL=" + kubectl, "KUSTOMIZE=" + kustomize,
		"KOTOKI_CLI=" + cli, "HOME=" + tmp}
	return env, logFile
}

func runDeploy(t *testing.T, env []string, args ...string) ([]string, error) {
	t.Helper()
	root := testutil.ModuleRoot(t)
	cmd := exec.Command("bash", append([]string{filepath.Join(root, "deploy/k8s/hack/deploy.sh")}, args...)...)
	cmd.Env = env
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	t.Logf("deploy.sh %v: %s", args, out)
	logFile := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "CALL_LOG="); ok {
			logFile = v
		}
	}
	var lines []string
	if b, rerr := os.ReadFile(logFile); rerr == nil {
		lines = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return lines, err
}

func indexOfCall(lines []string, parts ...string) int {
	for i, l := range lines {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return i
		}
	}
	return -1
}

func TestDeployScriptOrder(t *testing.T) {
	t.Run("staging runs migrate, seed, apply, rollout, smoke in order", func(t *testing.T) {
		env, _ := deployFixture(t)
		env = append(env, "SMOKE_EMAIL=s@example.test", "SMOKE_PASSWORD=pw")
		lines, err := runDeploy(t, env, "staging", testSHA)
		require.NoError(t, err)

		img := "ghcr.io/nabepi/kotoki-backend/api=ghcr.io/nabepi/kotoki-backend/api:" + testSHA
		require.GreaterOrEqual(t, indexOfCall(lines, "kustomize", "edit set image", img), 0, "tag = git SHA")
		for _, n := range []string{"realtime", "worker", "migrate", "seed"} {
			require.GreaterOrEqual(t, indexOfCall(lines, "edit set image", "/"+n+":"+testSHA), 0, n)
		}
		delMig := indexOfCall(lines, "delete job kotoki-migrate")
		applyMig := indexOfCall(lines, "apply", "component=migrate")
		waitMig := indexOfCall(lines, "wait", "condition=complete job/kotoki-migrate", "--timeout=600s")
		delSeed := indexOfCall(lines, "delete job kotoki-seed")
		applySeed := indexOfCall(lines, "apply", "component=seed")
		waitSeed := indexOfCall(lines, "wait", "condition=complete job/kotoki-seed")
		applyRest := indexOfCall(lines, "apply", "notin (migrate,seed)")
		sums := indexOfCall(lines, "kubectl", "get secret kotoki-api-secrets")
		roll := indexOfCall(lines, "rollout status", "deployment/kotoki-realtime")
		smoke := indexOfCall(lines, "kotoki-cli", "converse")
		seq := []int{delMig, applyMig, waitMig, delSeed, applySeed, waitSeed, applyRest, sums, roll, smoke}
		for i, v := range seq {
			require.GreaterOrEqual(t, v, 0, "call %d missing in %v", i, lines)
			if i > 0 {
				require.Greater(t, v, seq[i-1], "call %d out of order", i)
			}
		}
		for _, d := range []string{"api", "realtime", "worker-core", "worker-evaluation", "worker-memory", "worker-learner"} {
			require.GreaterOrEqual(t, indexOfCall(lines, "rollout status", "deployment/kotoki-"+d), 0, d)
		}
	})

	t.Run("failed migrate stops everything after it", func(t *testing.T) {
		env, _ := deployFixture(t)
		env = append(env, "STUB_FAIL_ON=condition=complete job/kotoki-migrate")
		lines, err := runDeploy(t, env, "staging", testSHA)
		require.Error(t, err)
		require.GreaterOrEqual(t, indexOfCall(lines, "wait", "job/kotoki-migrate"), 0)
		require.Equal(t, -1, indexOfCall(lines, "component=seed"), "seed must not run")
		require.Equal(t, -1, indexOfCall(lines, "notin"), "apply must not run")
		require.Equal(t, -1, indexOfCall(lines, "rollout status"), "rollout must not run")
		require.Equal(t, -1, indexOfCall(lines, "kotoki-cli"), "smoke must not run")
	})

	t.Run("failed seed stops before apply", func(t *testing.T) {
		env, _ := deployFixture(t)
		env = append(env, "STUB_FAIL_ON=condition=complete job/kotoki-seed")
		lines, err := runDeploy(t, env, "staging", testSHA)
		require.Error(t, err)
		require.Equal(t, -1, indexOfCall(lines, "notin"))
		require.Equal(t, -1, indexOfCall(lines, "rollout status"))
	})

	t.Run("failed rollout fails the deploy and skips smoke", func(t *testing.T) {
		env, _ := deployFixture(t)
		env = append(env, "SMOKE_EMAIL=s@example.test", "SMOKE_PASSWORD=pw", "STUB_FAIL_ON=rollout status deployment/kotoki-api")
		lines, err := runDeploy(t, env, "staging", testSHA)
		require.Error(t, err)
		require.Equal(t, -1, indexOfCall(lines, "kotoki-cli"))
	})

	t.Run("prod pins every image by digest", func(t *testing.T) {
		env, _ := deployFixture(t)
		dir := t.TempDir()
		for _, n := range []string{"api", "realtime", "worker", "migrate", "seed"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, n+".digest"), []byte(testDigest+"\n"), 0o644))
		}
		lines, err := runDeploy(t, env, "prod", testSHA, dir)
		require.NoError(t, err)
		for _, n := range []string{"api", "realtime", "worker", "migrate", "seed"} {
			want := "ghcr.io/nabepi/kotoki-backend/" + n + "=ghcr.io/nabepi/kotoki-backend/" + n + "@" + testDigest
			require.GreaterOrEqual(t, indexOfCall(lines, "edit set image", want), 0, n)
		}
		require.Equal(t, -1, indexOfCall(lines, "edit set image", ":"+testSHA), "prod must not use tags")
		require.Equal(t, -1, indexOfCall(lines, "kotoki-cli"), "no smoke account in prod")
		require.Less(t, indexOfCall(lines, "job/kotoki-migrate"), indexOfCall(lines, "notin"))
	})

	t.Run("refuses bad input before touching the cluster", func(t *testing.T) {
		digests := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(digests, "api.digest"), []byte("latest\n"), 0o644))
		for name, args := range map[string][]string{
			"unknown env":       {"dev", testSHA},
			"tag not sha":       {"staging", "latest"},
			"short sha":         {"staging", "abc123"},
			"prod no digests":   {"prod", testSHA},
			"prod bad digest":   {"prod", testSHA, digests},
			"prod missing file": {"prod", testSHA, t.TempDir()},
		} {
			env, logFile := deployFixture(t)
			_, err := runDeploy(t, env, args...)
			require.Error(t, err, name)
			_, statErr := os.Stat(logFile)
			require.True(t, os.IsNotExist(statErr), "%s: nothing may be invoked", name)
		}
	})

	t.Run("does not modify the working tree", func(t *testing.T) {
		env, _ := deployFixture(t)
		before := readRepoFile(t, "deploy", "k8s", "overlays", "staging", "kustomization.yaml")
		_, err := runDeploy(t, env, "staging", testSHA)
		require.NoError(t, err)
		require.Equal(t, before, readRepoFile(t, "deploy", "k8s", "overlays", "staging", "kustomization.yaml"))
	})
}

func TestSecretsChecksum(t *testing.T) {
	root := testutil.ModuleRoot(t)
	run := func(t *testing.T, v string) []string {
		env, logFile := deployFixture(t)
		env = append(env, "STUB_SECRET_V="+v)
		cmd := exec.Command("bash", filepath.Join(root, "deploy/k8s/hack/secrets-checksum.sh"), "staging")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		b, err := os.ReadFile(logFile)
		require.NoError(t, err)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	patches := func(lines []string) map[string]string {
		got := map[string]string{}
		re := regexp.MustCompile(`patch deployment (kotoki-[a-z-]+) .*"kotoki\.io/secrets-checksum":"([0-9a-f]{64})"`)
		for _, l := range lines {
			if m := re.FindStringSubmatch(l); m != nil {
				got[m[1]] = m[2]
			}
		}
		return got
	}
	a, b, a2 := patches(run(t, "v1")), patches(run(t, "v2")), patches(run(t, "v1"))
	require.Len(t, a, 6, "all six Deployments are patched")
	require.Equal(t, a, a2, "deterministic")
	require.NotEqual(t, a["kotoki-api"], b["kotoki-api"], "a changed Secret value changes the checksum")
	for _, v := range a {
		require.Equal(t, a["kotoki-api"], v, "one checksum for the whole set")
	}
	lines := run(t, "v1")
	require.NotContains(t, strings.Join(lines, "\n"), "=v1", "secret values never appear in calls")
}
