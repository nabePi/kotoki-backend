package config

import (
	"fmt"
	"strings"
)

var workerHandlerNames = map[string]bool{
	"outbox": true, "evaluation": true, "memory": true, "learner": true,
	"summary": true, "sweeper": true, "cleanup": true,
}

// ParseWorkerHandlers resolves the worker handler list; the flag wins over the
// WORKER_HANDLERS env value. Empty or unknown names are errors.
func ParseWorkerHandlers(flagValue, envValue string) ([]string, error) {
	src, name := flagValue, "--handlers"
	if strings.TrimSpace(flagValue) == "" {
		src, name = envValue, "WORKER_HANDLERS"
	}
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("WORKER_HANDLERS: empty (set --handlers or WORKER_HANDLERS)")
	}
	return splitHandlers(src, name)
}

func splitHandlers(src, name string) ([]string, error) {
	var out []string
	for _, p := range strings.Split(src, ",") {
		p = strings.TrimSpace(p)
		if !workerHandlerNames[p] {
			return nil, fmt.Errorf("WORKER_HANDLERS: unknown handler %q in %s", p, name)
		}
		out = append(out, p)
	}
	return out, nil
}
