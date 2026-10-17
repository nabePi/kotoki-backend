package chaos_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// TestEveryScenarioHasScript: spec 13.13.1 names test/chaos/<scenario>.sh. One script per scenario of
// the table (15: F14 is gone from v1), executable, parseable, and running its own scenario.
func TestEveryScenarioHasScript(t *testing.T) {
	scripts, err := filepath.Glob("f[0-9][0-9]-*.sh")
	require.NoError(t, err)
	require.Len(t, scripts, len(scenario.Table), "one script per scenario")
	byID := map[string]string{}
	for _, p := range scripts {
		m := regexp.MustCompile(`^f(\d\d)-[a-z0-9-]+\.sh$`).FindStringSubmatch(p)
		require.NotNil(t, m, p)
		id := "F" + strings.TrimPrefix(m[1], "0")
		require.NotContains(t, byID, id, "two scripts for %s", id)
		byID[id] = p

		st, err := os.Stat(p)
		require.NoError(t, err)
		require.NotZero(t, st.Mode().Perm()&0o100, "%s must be executable", p)
		out, err := exec.Command("bash", "-n", p).CombinedOutput()
		require.NoError(t, err, string(out))
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		require.Contains(t, string(b), "kotoki-chaos run "+id+" ", "%s runs its own scenario", p)
		require.Contains(t, string(b), "set -euo pipefail")
	}
	for _, s := range scenario.Table {
		require.Contains(t, byID, s.ID, "no script for %s", s.ID)
	}
	require.NotContains(t, byID, "F14")
}

type chaosDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Action   string `yaml:"action"`
		Mode     string `yaml:"mode"`
		Duration string `yaml:"duration"`
		Selector struct {
			Namespaces     []string          `yaml:"namespaces"`
			LabelSelectors map[string]string `yaml:"labelSelectors"`
		} `yaml:"selector"`
		Target *struct {
			Mode     string `yaml:"mode"`
			Selector struct {
				Namespaces     []string          `yaml:"namespaces"`
				LabelSelectors map[string]string `yaml:"labelSelectors"`
			} `yaml:"selector"`
		} `yaml:"target"`
	} `yaml:"spec"`
}

// TestChaosManifestsAreChaosMesh: the manifests cannot be applied here (no cluster, MANUAL GATE), so
// what can be checked is that each is a Chaos Mesh PodChaos/NetworkChaos that names a namespace of
// Kotoki, has a duration and cannot select everything (an empty selector would hit the whole cluster).
func TestChaosManifestsAreChaosMesh(t *testing.T) {
	files, err := filepath.Glob("k8s/*.yaml")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"k8s/network-rabbitmq-block.yaml", "k8s/rabbitmq-pod-kill.yaml", "k8s/redis-kill.yaml"}, files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		require.Contains(t, string(raw), "MANUAL GATE", "%s must say it is not exercised locally", f)
		var d chaosDoc
		require.NoError(t, yaml.Unmarshal(raw, &d), f)
		require.Equal(t, "chaos-mesh.org/v1alpha1", d.APIVersion, f)
		require.Contains(t, []string{"PodChaos", "NetworkChaos"}, d.Kind, f)
		require.Contains(t, []string{"kotoki", "kotoki-data"}, d.Metadata.Namespace, f)
		require.NotEmpty(t, d.Metadata.Name, f)
		require.NotEmpty(t, d.Spec.Duration, "%s: an experiment without duration never ends by itself", f)
		require.NotEmpty(t, d.Spec.Action, f)
		require.Contains(t, []string{"one", "all"}, d.Spec.Mode, f)
		require.NotEmpty(t, d.Spec.Selector.Namespaces, "%s: no selector namespace", f)
		for _, ns := range d.Spec.Selector.Namespaces {
			require.Contains(t, []string{"kotoki", "kotoki-data"}, ns, f)
		}
		require.NotEmpty(t, d.Spec.Selector.LabelSelectors, "%s: an empty label selector selects every pod", f)
		if d.Kind == "NetworkChaos" {
			require.NotNil(t, d.Spec.Target, "%s: a partition needs both sides", f)
			require.NotEmpty(t, d.Spec.Target.Selector.Namespaces, f)
			require.NotEmpty(t, d.Spec.Target.Selector.LabelSelectors, f)
		}
	}
}

// TestManifestsMatchTable: the table applies and deletes exactly the manifests that exist.
func TestManifestsMatchTable(t *testing.T) {
	used := map[string]bool{}
	for _, s := range scenario.Table {
		for _, m := range []map[scenario.Env][]scenario.Action{s.Inject, s.Revert} {
			for _, a := range m[scenario.Kube] {
				for _, arg := range a.Argv {
					if strings.HasPrefix(arg, "test/chaos/k8s/") {
						_, err := os.Stat(strings.TrimPrefix(arg, "test/chaos/"))
						require.NoError(t, err, "%s refers to %s", s.ID, arg)
						used[arg] = true
					}
				}
			}
		}
	}
	require.Contains(t, used, "test/chaos/k8s/redis-kill.yaml")
	require.Contains(t, used, "test/chaos/k8s/network-rabbitmq-block.yaml")
}
