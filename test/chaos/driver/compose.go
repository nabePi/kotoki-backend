package driver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// Compose performs KindCmd actions against the local stack: `docker compose restart|stop|start|pause|
// unpause <service>` and a `curl` to the RabbitMQ management API of this machine. Run executes argv;
// main makes it run with COMPOSE_PROJECT_NAME set, so that only the compose project of the run is
// touched. Resolve turns an `@token` argv element into its value at the moment of the action.
type Compose struct {
	Run     func(ctx context.Context, argv []string) error
	Resolve func(ctx context.Context, token string) (string, error)
}

var composeVerbs = []string{"restart", "stop", "start", "pause", "unpause"}

// Do implements scenario.Driver.
func (c Compose) Do(ctx context.Context, a scenario.Action) error {
	if a.Kind != scenario.KindCmd {
		return errNotCmd
	}
	if err := checkCompose(a.Argv); err != nil {
		return err
	}
	argv, err := resolve(ctx, a.Argv, c.Resolve)
	if err != nil {
		return err
	}
	if err := c.Run(ctx, argv); err != nil {
		return fmt.Errorf("driver: %s: %w", strings.Join(argvHead(a.Argv), " "), err)
	}
	return nil
}

// argvHead is the argv without the elements that may be long (tokens, bodies) for error messages.
func argvHead(argv []string) []string {
	if len(argv) > 4 {
		return argv[:4]
	}
	return argv
}

func resolve(ctx context.Context, argv []string, f func(context.Context, string) (string, error)) ([]string, error) {
	out := make([]string, len(argv))
	for i, s := range argv {
		if !strings.HasPrefix(s, "@") {
			out[i] = s
			continue
		}
		if f == nil {
			return nil, fmt.Errorf("driver: argv element %q needs a resolver", s)
		}
		v, err := f(ctx, s)
		if err != nil {
			return nil, fmt.Errorf("driver: resolve %q: %w", s, err)
		}
		out[i] = v
	}
	return out, nil
}

func checkCompose(argv []string) error {
	if len(argv) == 0 {
		return errors.New("driver: empty command")
	}
	switch argv[0] {
	case "docker":
		if len(argv) < 4 || argv[1] != "compose" || !slices.Contains(composeVerbs, argv[2]) {
			return fmt.Errorf("driver: only `docker compose %s <service>` is allowed, got %q", strings.Join(composeVerbs, "|"), strings.Join(argvHead(argv), " "))
		}
		for _, s := range argv[3:] {
			if strings.HasPrefix(s, "-") {
				return fmt.Errorf("driver: docker compose flag %q is not allowed", s)
			}
		}
		return nil
	case "curl":
		return checkCurl(argv)
	}
	return fmt.Errorf("driver: command %q is not allowed against compose", argv[0])
}

// checkCurl accepts one URL, on this machine, under the management API's exchanges.
func checkCurl(argv []string) error {
	var urls []string
	for _, s := range argv[1:] {
		if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			urls = append(urls, s)
		}
	}
	if len(urls) != 1 {
		return fmt.Errorf("driver: curl needs exactly one URL, got %d", len(urls))
	}
	u, err := url.Parse(urls[0])
	if err != nil {
		return fmt.Errorf("driver: curl URL: %w", err)
	}
	if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
		return fmt.Errorf("driver: curl may only talk to this machine, not %q", h)
	}
	// curl squashes dot segments, so a ".." (also percent-encoded) could climb out of /api/exchanges/
	if strings.Contains(strings.ToLower(u.EscapedPath()), "..") || strings.Contains(strings.ToLower(u.EscapedPath()), "%2e") {
		return fmt.Errorf("driver: curl path %q has dot segments", u.Path)
	}
	if !strings.HasPrefix(u.EscapedPath(), "/api/exchanges/") {
		return fmt.Errorf("driver: curl may only use the exchanges of the RabbitMQ management API, not %q", u.Path)
	}
	return nil
}
