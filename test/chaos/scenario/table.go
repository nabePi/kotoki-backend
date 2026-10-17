package scenario

import (
	"time"

	"github.com/nabePi/kotoki-backend/test/chaos/faultdoc"
)

// F13Token is the argv element that the driver replaces with the management-API publish body of a
// session.completed message that was already published (spec 13.13.2 F13: the same event again).
const F13Token = "@republish:session.completed"

// ProbeConverseDrop is the probe of F16: one conversation whose connection is dropped and resumed.
const ProbeConverseDrop = "converse-drop"

// ProbeExpirySpeaking is the probe of F15: a 300 s session whose user speaks just before it expires.
const ProbeExpirySpeaking = "expiry-speaking"

func setFault(f map[string]faultdoc.Fault) []Action { return []Action{{Kind: KindFault, Fault: f}} }

func clearFault() []Action { return []Action{{Kind: KindFault}} }

func cmd(argv ...string) Action { return Action{Kind: KindCmd, Argv: argv} }

// both applies the same actions to both environments: the fake provider faults go through Redis in
// either.
func both(a []Action) map[Env][]Action { return map[Env][]Action{Compose: a, Kube: a} }

const (
	// window of the "increase" assertions: longer than any run, so that each reads the whole run.
	run = "[30m]"
	// recovery is the trailing window of the "after the revert" assertions; the load keeps running
	// for longer than this after the revert.
	recovery = "[1m]"
)

func increase(metric, sel string) string {
	return "sum(increase(" + metric + sel + run + "))"
}

func positive(name, expr string) Assertion {
	return Assertion{Kind: AssertProm, Name: name, Expr: expr, Cmp: ">", Want: 0}
}

func zero(name, expr string) Assertion {
	return Assertion{Kind: AssertProm, Name: name, Expr: expr, Cmp: "==", Want: 0}
}

func integrity() Assertion {
	return Assertion{Kind: AssertVerify, Name: "kotoki-verify: 0 violations"}
}

// turnsRecovered: turns finished normally during the last minute, i.e. after the fault was removed.
func turnsRecovered() Assertion {
	return positive("normal turns after the revert",
		`sum(increase(kotoki_realtime_turns_total{status="COMPLETED"}`+recovery+`))`)
}

// Table is the failure-injection catalogue of spec 13.13.2. F14 was removed from v1 and the
// numbers are not shifted.
var Table = []Scenario{
	{
		ID: "F1", Title: "STT timeout in the middle of an utterance", LocalOK: true,
		Inject: both(setFault(map[string]faultdoc.Fault{"STT.FAKE_PRIMARY": {Mode: "timeout", Rate: 1.0, AfterMS: 2000}})),
		Revert: both(clearFault()),
		Expect: []Assertion{
			positive("provider_errors_total{STT} rose", increase("kotoki_provider_errors_total", `{capability="STT"}`)),
			zero("no vendor switch for STT (D-017)", increase("kotoki_provider_fallback_total", `{capability="STT"}`)+" or vector(0)"),
			positive("sessions ended PROVIDER_FAILURE", increase("kotoki_sessions_ended_total", `{end_reason="PROVIDER_FAILURE"}`)),
			turnsRecovered(),
			integrity(),
		},
	},
	{
		ID: "F2", Title: "LLM first-token timeout", LocalOK: true,
		Inject: both(setFault(map[string]faultdoc.Fault{"LLM.FAKE_PRIMARY": {Mode: "timeout", Rate: 1.0}})),
		Revert: both(clearFault()),
		Expect: []Assertion{
			positive("provider_errors_total{LLM} rose", increase("kotoki_provider_errors_total", `{capability="LLM"}`)),
			positive("turns ended FAILED", increase("kotoki_realtime_turns_total", `{status="FAILED"}`)),
			turnsRecovered(),
			integrity(),
		},
	},
	{
		ID: "F3", Title: "TTS fails after the first audio", LocalOK: true,
		Inject: both(setFault(map[string]faultdoc.Fault{"TTS.FAKE_PRIMARY": {Mode: "fail_after_first_audio", Rate: 1.0}})),
		Revert: both(clearFault()),
		Expect: []Assertion{
			positive("provider_errors_total{TTS} rose", increase("kotoki_provider_errors_total", `{capability="TTS"}`)),
			positive("turns ended FAILED", increase("kotoki_realtime_turns_total", `{status="FAILED"}`)),
			turnsRecovered(),
			integrity(),
		},
	},
	{
		ID: "F4", Title: "Active STT/LLM vendor down for 2 minutes", LocalOK: true,
		Inject: both(setFault(map[string]faultdoc.Fault{
			"STT.FAKE_PRIMARY": {Mode: "error", Rate: 1.0},
			"LLM.FAKE_PRIMARY": {Mode: "error", Rate: 1.0},
		})),
		Revert: both(clearFault()),
		Expect: []Assertion{
			// counters, not samples of the gauge: HALF_OPEN lasts as long as one probe call
			positive("circuit opened", increase("kotoki_provider_circuit_transitions_total", `{to="OPEN"}`)),
			positive("circuit went half-open", increase("kotoki_provider_circuit_transitions_total", `{to="HALF_OPEN"}`)),
			positive("circuit closed again", increase("kotoki_provider_circuit_transitions_total", `{to="CLOSED"}`)),
			zero("every circuit CLOSED again", `max(kotoki_provider_circuit_state)`),
			turnsRecovered(),
			integrity(),
			{Kind: AssertProm, Name: "alert ProviderCircuitOpen fired", Only: Kube, Cmp: ">", Want: 0,
				Expr: `max(max_over_time(ALERTS{alertname="ProviderCircuitOpen",alertstate="firing"}` + run + `))`},
		},
	},
	{
		ID: "F5", Title: "Every TTS vendor of the character down", LocalOK: true,
		Inject: both(setFault(map[string]faultdoc.Fault{
			"TTS.FAKE_PRIMARY":  {Mode: "error", Rate: 1.0},
			"TTS.FAKE_FALLBACK": {Mode: "error", Rate: 1.0},
		})),
		Revert: both(clearFault()),
		Expect: []Assertion{
			positive("provider_errors_total{TTS} rose", increase("kotoki_provider_errors_total", `{capability="TTS"}`)),
			turnsRecovered(),
			integrity(),
			{Kind: AssertProm, Name: "alert ProviderUnavailable fired", Only: Kube, Cmp: ">", Want: 0,
				Expr: `max(max_over_time(ALERTS{alertname="ProviderUnavailable",alertstate="firing"}` + run + `))`},
		},
	},
	{
		ID: "F6", Title: "Redis restart / failover", LocalOK: true,
		Inject: map[Env][]Action{
			Compose: {cmd("docker", "compose", "restart", "redis")},
			Kube:    {cmd("kubectl", "apply", "-f", "test/chaos/k8s/redis-kill.yaml")},
		},
		Revert: map[Env][]Action{
			Kube: {cmd("kubectl", "delete", "-f", "test/chaos/k8s/redis-kill.yaml", "--ignore-not-found")},
		},
		Expect: []Assertion{
			// the restart must have been seen by the pods, or "nothing broke" proves nothing
			positive("the pods saw Redis fail", increase("kotoki_redis_command_errors_total", "")),
			zero("no false heartbeat_lost (rule b)", increase("kotoki_sweeper_actions_total", `{rule="b",result="ok"}`)+" or vector(0)"),
			turnsRecovered(),
			integrity(),
		},
	},
	{
		ID: "F7", Title: "One of three RabbitMQ nodes down",
		Inject: map[Env][]Action{Kube: {cmd("kubectl", "delete", "pod", "kotoki-rabbitmq-server-0", "-n", "kotoki-data")}},
		Expect: []Assertion{
			{Kind: AssertProm, Name: "outbox lag stays under 30 s", Cmp: "<", Want: 30,
				Expr: `max(max_over_time(kotoki_outbox_oldest_pending_age_seconds` + run + `))`},
			integrity(),
		},
	},
	{
		ID: "F8", Title: "RabbitMQ down for 10 minutes", LocalOK: true,
		Inject: map[Env][]Action{
			Compose: {cmd("docker", "compose", "stop", "rabbitmq")},
			Kube:    {cmd("kubectl", "apply", "-f", "test/chaos/k8s/network-rabbitmq-block.yaml")},
		},
		Revert: map[Env][]Action{
			Compose: {cmd("docker", "compose", "start", "rabbitmq")},
			Kube:    {cmd("kubectl", "delete", "-f", "test/chaos/k8s/network-rabbitmq-block.yaml", "--ignore-not-found")},
		},
		Expect: []Assertion{
			positive("outbox_pending rose", `max(max_over_time(kotoki_outbox_pending`+run+`))`),
			zero("outbox_pending drained to 0", `max(kotoki_outbox_pending)`),
			integrity(),
			{Kind: AssertProm, Name: "alert OutboxLagCritical fired", Only: Kube, Cmp: ">", Want: 0,
				Expr: `max(max_over_time(ALERTS{alertname="OutboxLagCritical",alertstate="firing"}` + run + `))`},
		},
	},
	{
		ID: "F9", Title: "Realtime pod killed during sessions",
		Inject: map[Env][]Action{Kube: {cmd("kubectl", "-n", "kotoki", "delete", "pod", "@pod:app.kubernetes.io/component=realtime", "--grace-period=0", "--force")}},
		Expect: []Assertion{
			positive("reconnects took over", increase("kotoki_realtime_reconnect_total", `{result="takeover"}`)),
			{Kind: AssertProm, Name: "reconnect gap P95 under 30 s", Cmp: "<", Want: 30,
				Expr: `histogram_quantile(0.95, sum by (le) (increase(kotoki_realtime_reconnect_gap_seconds_bucket` + run + `)))`},
			integrity(),
			{Kind: AssertManual, Name: "at least 99% of the affected sessions continued (kotoki-load resume_ok_ratio)"},
		},
	},
	{
		ID: "F10", Title: "Rolling restart of realtime under load",
		Inject: map[Env][]Action{Kube: {cmd("kubectl", "-n", "kotoki", "rollout", "restart", "deployment/kotoki-realtime")}},
		Revert: map[Env][]Action{Kube: {cmd("kubectl", "-n", "kotoki", "rollout", "status", "deployment/kotoki-realtime", "--timeout=10m")}},
		Expect: []Assertion{
			zero("no session ended SERVER_ERROR", increase("kotoki_sessions_ended_total", `{end_reason="SERVER_ERROR"}`)+" or vector(0)"),
			{Kind: AssertProm, Name: "reconnect gap P95 under 5 s", Cmp: "<", Want: 5,
				Expr: `histogram_quantile(0.95, sum by (le) (increase(kotoki_realtime_reconnect_gap_seconds_bucket` + run + `)))`},
			integrity(),
		},
	},
	{
		ID: "F11", Title: "PostgreSQL failover",
		Inject: map[Env][]Action{Kube: {cmd("kubectl", "-n", "kotoki-data", "delete", "pod", "-l", "cnpg.io/instanceRole=primary")}},
		Expect: []Assertion{
			integrity(),
			{Kind: AssertManual, Name: "managed PostgreSQL: trigger the provider's failover instead of deleting a CNPG pod"},
		},
	},
	{
		ID: "F12", Title: "Evaluation worker killed while processing",
		Inject: map[Env][]Action{Kube: {cmd("kubectl", "-n", "kotoki", "delete", "pod", "@pod:app.kubernetes.io/component=worker-evaluation", "--grace-period=0", "--force")}},
		Expect: []Assertion{
			integrity(), // includes V8 and the exactly-one-evaluation-per-session invariant
		},
	},
	{
		ID: "F13", Title: "Duplicate message", LocalOK: true,
		// real time: the event to repeat must be published and 20 s old (a session that ended)
		Fixed: &Window{InjectAt: 90 * time.Second, RevertAt: 2 * time.Minute, Tail: 2 * time.Minute},
		Inject: map[Env][]Action{
			Compose: {cmd("curl", "-fsS", "-u", "kotoki:kotoki", "-XPOST", "-H", "content-type: application/json",
				"http://localhost:15672/api/exchanges/%2F/kotoki.events/publish", "-d", F13Token)},
		},
		Expect: []Assertion{
			positive("consumers skipped the duplicate", increase("kotoki_consumer_messages_total", `{result="duplicate"}`)),
			integrity(),
		},
	},
	{
		ID: "F15", Title: "Session runs out while the AI speaks", LocalOK: true,
		// real time: the probe's 300 s session starts at once and the inject ends when it completes
		Fixed:  &Window{InjectAt: 5 * time.Second, RevertAt: 10 * time.Second, Tail: 5 * time.Second},
		Inject: map[Env][]Action{Compose: {{Kind: KindProbe, Argv: []string{ProbeExpirySpeaking}}}},
		Expect: []Assertion{
			positive("sessions ended by DURATION_ELAPSED", increase("kotoki_sessions_ended_total", `{end_reason="DURATION_ELAPSED"}`)),
			integrity(), // charge <= reserved, ledger consistent (V1-V8)
		},
	},
	{
		ID: "F16", Title: "Client network drops and returns", LocalOK: true,
		Inject: map[Env][]Action{Compose: {{Kind: KindProbe, Argv: []string{ProbeConverseDrop}}}},
		Expect: []Assertion{
			positive("reconnects served", increase("kotoki_realtime_reconnect_total", `{result="same_pod"}`)),
			integrity(),
		},
	},
}
