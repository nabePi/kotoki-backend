package driver_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/test/chaos/driver"
	"github.com/nabePi/kotoki-backend/test/chaos/faultdoc"
	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// ---- redis ------------------------------------------------------------------------------------

type kvCall struct {
	op, key string
	val     []byte
	ttl     time.Duration
}

type fakeKV struct {
	mu    sync.Mutex
	calls []kvCall
	err   error
}

func (k *fakeKV) Set(_ context.Context, key string, v []byte, ttl time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, kvCall{"SET", key, v, ttl})
	return k.err
}

func (k *fakeKV) DelPattern(_ context.Context, pattern string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, kvCall{op: "DELPATTERN", key: pattern})
	return 3, k.err
}

func (k *fakeKV) Del(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, kvCall{op: "DEL", key: key})
	return k.err
}

func TestRedisDriver_SetsAndDeletesKey(t *testing.T) {
	kv := &fakeKV{}
	r := driver.Redis{KV: kv, Until: 30 * time.Minute, Now: func() time.Time { return now }}
	f := map[string]faultdoc.Fault{"STT.FAKE_PRIMARY": {Mode: "timeout", Rate: 1, AfterMS: 2000}}
	require.NoError(t, r.Do(t.Context(), scenario.Action{Kind: scenario.KindFault, Fault: f}))
	require.NoError(t, r.Do(t.Context(), scenario.Action{Kind: scenario.KindFault}))

	require.Len(t, kv.calls, 2)
	require.Equal(t, "SET", kv.calls[0].op)
	require.Equal(t, "kotoki:fake:faults", kv.calls[0].key)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(kv.calls[0].val, &doc))
	require.Equal(t, "2026-10-17T12:30:00Z", doc["until"], "the document expires by itself if the revert never runs")
	require.Equal(t, 30*time.Minute, kv.calls[0].ttl, "so does the key")
	require.Equal(t, kvCall{op: "DEL", key: "kotoki:fake:faults"}, kv.calls[1])
}

func TestRedisDriver_ResetEmptiesOnlyTheSessionStartBudgets(t *testing.T) {
	kv := &fakeKV{}
	r := driver.Redis{KV: kv}
	require.NoError(t, r.Do(t.Context(), scenario.Action{Kind: scenario.KindReset, Argv: []string{scenario.ResetRateLimits}}))
	require.Equal(t, []kvCall{
		{op: "DELPATTERN", key: "kotoki:ratelimit:session_create_user:*"},
		{op: "DELPATTERN", key: "kotoki:ratelimit:ticket_user:*"},
		{op: "DELPATTERN", key: "kotoki:ratelimit:ws_connect_ip:*"},
		{op: "DELPATTERN", key: "kotoki:ratelimit:ws_attach_user:*"},
	}, kv.calls)

	for _, argv := range [][]string{nil, {"everything"}, {"ratelimits", "extra"}, {"*"}} {
		require.Error(t, r.Do(t.Context(), scenario.Action{Kind: scenario.KindReset, Argv: argv}), "%v", argv)
	}
	require.Len(t, kv.calls, 4, "an unknown reset deletes nothing")
	kv.err = errors.New("redis down")
	require.Error(t, r.Do(t.Context(), scenario.Action{Kind: scenario.KindReset, Argv: []string{scenario.ResetRateLimits}}))
}

func TestRedisDriver_CustomKeyAndErrors(t *testing.T) {
	kv := &fakeKV{err: errors.New("redis down")}
	r := driver.Redis{KV: kv, Key: "other:key", Now: func() time.Time { return now }}
	require.Error(t, r.Do(t.Context(), scenario.Action{Kind: scenario.KindFault}))
	require.Equal(t, "other:key", kv.calls[0].key)
	require.Error(t, r.Do(t.Context(), scenario.Action{Kind: scenario.KindCmd}), "only fault actions")
	bad := scenario.Action{Kind: scenario.KindFault, Fault: map[string]faultdoc.Fault{"STT.ELEVENLABS": {Mode: "error", Rate: 1}}}
	require.Error(t, r.Do(t.Context(), bad), "an invalid document is refused before it reaches Redis")
	require.Len(t, kv.calls, 1)
}

// TestRedisDriver_DocumentsAreAcceptedByTheFakeProvider parses what every table scenario writes with
// the parser of the fake provider itself, which skips what it does not understand: a scenario whose
// fault is silently skipped would pass for the wrong reason.
func TestRedisDriver_DocumentsAreAcceptedByTheFakeProvider(t *testing.T) {
	for _, sc := range scenario.Table {
		for env, as := range sc.Inject {
			for _, a := range as {
				if a.Kind != scenario.KindFault || len(a.Fault) == 0 {
					continue
				}
				kv := &fakeKV{}
				require.NoError(t, driver.Redis{KV: kv, Until: time.Hour, Now: func() time.Time { return now }}.Do(t.Context(), a))
				var warns []string
				set := fake.ParseFaultSet(kv.calls[0].val, now, func(s string) { warns = append(warns, s) })
				require.Empty(t, warns, "%s/%s", sc.ID, env)
				require.Equal(t, len(a.Fault), set.Len(), "%s/%s", sc.ID, env)
				for key := range a.Fault {
					capName, inst, _ := strings.Cut(key, ".")
					_, ok := set.Lookup(provider.Capability(capName), provider.Name(inst))
					require.True(t, ok, "%s/%s: %s", sc.ID, env, key)
				}
			}
		}
	}
}

// ---- compose ----------------------------------------------------------------------------------

type recorder struct {
	mu   sync.Mutex
	argv [][]string
	err  error
}

func (r *recorder) Run(_ context.Context, argv []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argv = append(r.argv, argv)
	return r.err
}

func cmdAction(argv ...string) scenario.Action {
	return scenario.Action{Kind: scenario.KindCmd, Argv: argv}
}

func TestComposeDriver_Argv(t *testing.T) {
	rec := &recorder{}
	c := driver.Compose{Run: rec.Run}
	for _, id := range []string{"F6", "F8"} {
		sc, _ := scenario.ByID(id)
		for _, a := range sc.Inject[scenario.Compose] {
			require.NoError(t, c.Do(t.Context(), a))
		}
		for _, a := range sc.Revert[scenario.Compose] {
			require.NoError(t, c.Do(t.Context(), a))
		}
	}
	require.Equal(t, [][]string{
		{"docker", "compose", "restart", "redis"},
		{"docker", "compose", "stop", "rabbitmq"},
		{"docker", "compose", "start", "rabbitmq"},
	}, rec.argv)
}

func TestComposeDriver_RefusesWhatItWasNotMadeFor(t *testing.T) {
	c := driver.Compose{Run: (&recorder{}).Run}
	for _, argv := range [][]string{
		{"docker", "compose", "down", "-v"},
		{"docker", "compose", "down", "redis"}, // the verb alone, with a service: still not a restart/stop/start
		{"docker", "compose", "rm", "-f"},
		{"docker", "compose", "rm", "redis"},
		{"docker", "compose", "restart", "--no-deps", "redis"},
		{"docker", "compose", "up", "redis"},
		{"docker", "rm", "-f", "abc"},
		{"docker", "kill", "abc"},
		{"rm", "-rf", "/"},
		{"curl", "http://example.com/x"},
		{"curl", "-fsS", "https://evil.test/api/exchanges/x"},   // right path, wrong host
		{"curl", "-fsS", "http://localhost:15672/api/queues/x"}, // right host, wrong path
		{"curl", "-fsS", "http://localhost:15672/api/vhosts"},
		{"curl", "-fsS", "http://localhost:15672/api/exchanges/x", "http://example.com/api/exchanges/y"}, // two URLs
		{"curl", "-fsS", "http://localhost:15672/api/exchanges/../users"},
		{"docker", "compose"},
		{},
	} {
		require.Error(t, c.Do(t.Context(), cmdAction(argv...)), "%v", argv)
	}
	require.Error(t, c.Do(t.Context(), scenario.Action{Kind: scenario.KindFault}))
}

func TestComposeDriver_RunErrorIsReturned(t *testing.T) {
	c := driver.Compose{Run: (&recorder{err: errors.New("exit 1")}).Run}
	require.ErrorContains(t, c.Do(t.Context(), cmdAction("docker", "compose", "restart", "redis")), "exit 1")
}

func TestComposeDriver_RepublishResolvesTheTokenAtRunTime(t *testing.T) {
	rec := &recorder{}
	sc, _ := scenario.ByID("F13")
	a := sc.Inject[scenario.Compose][0]
	// the argv of the table names the management API and the routing key
	joined := strings.Join(a.Argv, " ")
	require.Contains(t, joined, "/api/exchanges/%2F/kotoki.events/publish")
	require.Contains(t, joined, "session.completed")

	var asked string
	c := driver.Compose{Run: rec.Run, Resolve: func(_ context.Context, token string) (string, error) {
		asked = token
		return `{"routing_key":"session.completed"}`, nil
	}}
	require.NoError(t, c.Do(t.Context(), a))
	require.Equal(t, scenario.F13Token, asked)
	last := rec.argv[0][len(rec.argv[0])-1]
	require.JSONEq(t, `{"routing_key":"session.completed"}`, last)

	noResolver := driver.Compose{Run: rec.Run}
	require.Error(t, noResolver.Do(t.Context(), a), "a token with nothing to resolve it must not be sent literally")
}

// ---- kube -------------------------------------------------------------------------------------

type kubeOut struct {
	argv [][]string
	out  string
	err  error
}

func (k *kubeOut) Output(_ context.Context, argv []string) (string, error) {
	k.argv = append(k.argv, argv)
	return k.out, k.err
}

func TestKubeDriver_Argv(t *testing.T) {
	rec := &recorder{}
	out := &kubeOut{out: "pod/kotoki-realtime-7d9f-abcde\npod/kotoki-realtime-7d9f-zzzzz\n"}
	k := driver.Kube{Run: rec.Run, Output: out.Output, Namespace: "kotoki"}
	for _, id := range []string{"F6", "F7", "F9", "F10", "F11", "F12"} {
		sc, _ := scenario.ByID(id)
		for _, a := range sc.Inject[scenario.Kube] {
			if a.Kind == scenario.KindCmd {
				out.out = "pod/kotoki-realtime-7d9f-abcde\npod/kotoki-realtime-7d9f-zzzzz\n"
				require.NoError(t, k.Do(t.Context(), a), id)
			}
		}
	}
	got := make([]string, len(rec.argv))
	for i, a := range rec.argv {
		got[i] = strings.Join(a, " ")
	}
	require.Contains(t, got, "kubectl apply -f test/chaos/k8s/redis-kill.yaml")
	require.Contains(t, got, "kubectl delete pod kotoki-rabbitmq-server-0 -n kotoki-data")
	require.Contains(t, got, "kubectl -n kotoki delete pod kotoki-realtime-7d9f-abcde --grace-period=0 --force")
	require.Contains(t, got, "kubectl -n kotoki rollout restart deployment/kotoki-realtime")
	require.Contains(t, got, "kubectl -n kotoki-data delete pod -l cnpg.io/instanceRole=primary")
	// the pod was looked up in the namespace of the command, among running pods, by the label
	require.Equal(t, []string{"kubectl", "-n", "kotoki", "get", "pods", "-l", "app.kubernetes.io/component=worker-evaluation",
		"--field-selector=status.phase=Running", "-o", "name"}, out.argv[len(out.argv)-1])
}

func TestKubeDriver_PodLookupFailures(t *testing.T) {
	a := cmdAction("kubectl", "-n", "kotoki", "delete", "pod", "@pod:app=x", "--force")
	rec := &recorder{}
	require.Error(t, driver.Kube{Run: rec.Run, Output: (&kubeOut{out: ""}).Output}.Do(t.Context(), a), "no pod is an error, never a silent no-op")
	require.Error(t, driver.Kube{Run: rec.Run, Output: (&kubeOut{err: errors.New("boom")}).Output}.Do(t.Context(), a))
	require.Empty(t, rec.argv)
}

func TestKubeDriver_RefusesWhatItWasNotMadeFor(t *testing.T) {
	k := driver.Kube{Run: (&recorder{}).Run, Output: (&kubeOut{}).Output}
	for _, argv := range [][]string{
		{"kubectl", "delete", "namespace", "kotoki"},
		{"kubectl", "delete", "pod", "--all", "-n", "kotoki"},
		{"kubectl", "delete", "pod", "-n", "kube-system", "x"},
		{"kubectl", "drain", "node-1"},
		{"kubectl", "apply", "-f", "test/chaos/k8s/../../../etc/x.yaml"},
		{"kubectl", "apply", "-f", "deploy/k8s/base/namespace.yaml"},
		{"docker", "compose", "restart", "redis"},
		{"kubectl"},
	} {
		require.Error(t, k.Do(t.Context(), cmdAction(argv...)), "%v", argv)
	}
}

// ---- multi ------------------------------------------------------------------------------------

type tag struct {
	name string
	got  *[]string
}

func (t tag) Do(_ context.Context, a scenario.Action) error {
	*t.got = append(*t.got, t.name+":"+a.Kind)
	return nil
}

func TestMultiDriver_RoutesByKind(t *testing.T) {
	var got []string
	m := driver.Multi{Fault: tag{"fault", &got}, Cmd: tag{"cmd", &got}, Probe: tag{"probe", &got}}
	for _, k := range []string{scenario.KindFault, scenario.KindCmd, scenario.KindProbe, scenario.KindReset} {
		require.NoError(t, m.Do(t.Context(), scenario.Action{Kind: k}))
	}
	require.Equal(t, []string{"fault:fault", "cmd:cmd", "probe:probe", "fault:reset"}, got, "a reset goes to the Redis driver")
	require.Error(t, m.Do(t.Context(), scenario.Action{Kind: "weird"}))
	require.Error(t, driver.Multi{}.Do(t.Context(), scenario.Action{Kind: scenario.KindFault}), "a missing driver is an error, not a no-op")
}

// ---- probe ------------------------------------------------------------------------------------

func TestProbeDriver_RunsTheNamedProbeWithItsArguments(t *testing.T) {
	var got []string
	p := driver.Probe{Funcs: map[string]func(context.Context, []string) error{
		"one": func(_ context.Context, args []string) error { got = args; return nil },
		"bad": func(context.Context, []string) error { return errors.New("probe failed") },
	}}
	require.NoError(t, p.Do(t.Context(), scenario.Action{Kind: scenario.KindProbe, Argv: []string{"one", "a", "b"}}))
	require.Equal(t, []string{"a", "b"}, got)
	require.ErrorContains(t, p.Do(t.Context(), scenario.Action{Kind: scenario.KindProbe, Argv: []string{"bad"}}), "probe failed")
	require.Error(t, p.Do(t.Context(), scenario.Action{Kind: scenario.KindProbe, Argv: []string{"nope"}}))
	require.Error(t, p.Do(t.Context(), scenario.Action{Kind: scenario.KindProbe}))
	require.Error(t, p.Do(t.Context(), scenario.Action{Kind: scenario.KindCmd, Argv: []string{"one"}}))
}
