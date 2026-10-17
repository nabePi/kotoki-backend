package driver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// Kube performs KindCmd actions against the cluster with kubectl: delete one pod (by name or label,
// in the namespaces of Kotoki), rollout restart/status of a deployment, and apply/delete of a Chaos
// Mesh manifest of test/chaos/k8s. `@pod:<label selector>` in argv becomes the first running pod
// with that label, looked up in the namespace of the command.
type Kube struct {
	Run       func(ctx context.Context, argv []string) error
	Output    func(ctx context.Context, argv []string) (string, error)
	Namespace string // namespace of a pod lookup whose command names none (default kotoki)
}

var kubeNamespaces = []string{"kotoki", "kotoki-data"}

const manifestDir = "test/chaos/k8s/"

// Do implements scenario.Driver.
func (k Kube) Do(ctx context.Context, a scenario.Action) error {
	if a.Kind != scenario.KindCmd {
		return errNotCmd
	}
	if err := checkKube(a.Argv); err != nil {
		return err
	}
	argv := slices.Clone(a.Argv)
	for i, s := range argv {
		sel, ok := strings.CutPrefix(s, "@pod:")
		if !ok {
			continue
		}
		ns := namespaceOf(argv)
		if ns == "" {
			ns = k.Namespace
		}
		if ns == "" {
			ns = "kotoki"
		}
		pod, err := k.firstPod(ctx, ns, sel)
		if err != nil {
			return err
		}
		argv[i] = pod
	}
	if err := k.Run(ctx, argv); err != nil {
		return fmt.Errorf("driver: %s: %w", strings.Join(argvHead(a.Argv), " "), err)
	}
	return nil
}

func (k Kube) firstPod(ctx context.Context, ns, selector string) (string, error) {
	out, err := k.Output(ctx, []string{"kubectl", "-n", ns, "get", "pods", "-l", selector,
		"--field-selector=status.phase=Running", "-o", "name"})
	if err != nil {
		return "", fmt.Errorf("driver: list pods %q: %w", selector, err)
	}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return strings.TrimPrefix(l, "pod/"), nil
		}
	}
	return "", fmt.Errorf("driver: no running pod matches %q in %s", selector, ns)
}

func namespaceOf(argv []string) string {
	for i, s := range argv {
		if (s == "-n" || s == "--namespace") && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func checkKube(argv []string) error {
	if len(argv) < 2 || argv[0] != "kubectl" {
		return fmt.Errorf("driver: only kubectl is allowed against a cluster, got %q", strings.Join(argvHead(argv), " "))
	}
	ns := namespaceOf(argv)
	var rest []string // argv without -n <ns>
	for i := 1; i < len(argv); i++ {
		if argv[i] == "-n" || argv[i] == "--namespace" {
			i++
			continue
		}
		rest = append(rest, argv[i])
	}
	nsOK := ns != "" && slices.Contains(kubeNamespaces, ns)
	switch {
	case len(rest) >= 3 && rest[0] == "delete" && rest[1] == "pod":
		if !nsOK {
			return fmt.Errorf("driver: deleting a pod needs -n %s, got %q", strings.Join(kubeNamespaces, "|"), ns)
		}
		if slices.Contains(rest, "--all") || slices.Contains(rest, "-A") || slices.Contains(rest, "--all-namespaces") {
			return errors.New("driver: --all is not allowed")
		}
		if rest[2] == "-l" && len(rest) < 4 {
			return errors.New("driver: -l needs a selector")
		}
		return nil
	case len(rest) >= 3 && rest[0] == "rollout" && (rest[1] == "restart" || rest[1] == "status") && strings.HasPrefix(rest[2], "deployment/"):
		if ns != "kotoki" {
			return fmt.Errorf("driver: rollout is only for namespace kotoki, got %q", ns)
		}
		return nil
	case len(rest) >= 3 && (rest[0] == "apply" || rest[0] == "delete") && rest[1] == "-f" && strings.HasPrefix(rest[2], manifestDir) && !strings.Contains(rest[2], ".."):
		return nil
	}
	return fmt.Errorf("driver: kubectl command %q is not allowed", strings.Join(argvHead(argv), " "))
}
