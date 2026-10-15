// Layout rules (D-005, spec 2.2) mirrored from the depguard config as a
// table-driven safety net.
package arch_test

import "strings"

const modulePath = "github.com/nabePi/kotoki-backend/"

var (
	featurePrefixes = []string{
		"internal/auth", "internal/balance", "internal/session", "internal/content",
		"internal/conversation", "internal/prompt", "internal/provider",
		"internal/evaluation", "internal/memory", "internal/learner",
		"internal/summary", "internal/admin", "internal/events",
	}
	vendorExempt = []string{"internal/provider/openai", "internal/provider/gemini", "internal/provider/azure"}
	adapters     = []string{
		"internal/postgres", "internal/redisstore", "internal/amqp", "internal/objectstore",
		"internal/mailer", "internal/provider/elevenlabs", "internal/provider/deepgram",
		"internal/provider/openai", "internal/provider/gemini", "internal/provider/azure",
	}
	handlers     = []string{"internal/httpapi", "internal/wsgateway"}
	audioPkg     = "internal/conversation/audio"
	audioAllowed = []string{"cmd/realtime", "cmd/kotoki-cli", "internal/conversation", "internal/wsgateway", "internal/kotokiclient",
		"internal/testutil/audiofx", "test"} // audiofx: §14.9.5 exception; the internal/testutil root stays cgo-free

	featureDenyExternal = []string{
		"github.com/go-chi/chi/v5", "github.com/jackc/pgx/v5", "github.com/pgvector/pgvector-go",
		"github.com/redis/go-redis/v9", "github.com/rabbitmq/amqp091-go", "github.com/coder/websocket",
		"github.com/aws/aws-sdk-go-v2",
	}
	featureDenyInternal = []string{
		"internal/postgres", "internal/redisstore", "internal/amqp", "internal/objectstore",
		"internal/mailer", "internal/httpapi", "internal/wsgateway", "internal/jobs",
		"internal/provider/elevenlabs", "internal/provider/deepgram", "internal/provider/openai",
		"internal/provider/gemini", "internal/provider/azure",
	}
	handlerDenyExternal = []string{
		"github.com/jackc/pgx/v5", "github.com/pgvector/pgvector-go", "github.com/redis/go-redis/v9",
		"github.com/rabbitmq/amqp091-go", "github.com/aws/aws-sdk-go-v2",
	}
	handlerDenyInternal = []string{
		"internal/postgres", "internal/redisstore", "internal/amqp", "internal/objectstore", "internal/jobs",
	}
	adapterDenyInternal = []string{"internal/httpapi", "internal/wsgateway", "internal/jobs"}
	cgoLibs             = []string{"gopkg.in/hraban/opus.v2", "github.com/maxhawkins/go-webrtcvad"}
)

// within reports whether rel equals prefix or is below it.
func within(rel, prefix string) bool {
	return rel == prefix || strings.HasPrefix(rel, prefix+"/")
}

func withinAny(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if within(rel, p) {
			return true
		}
	}
	return false
}

// isFeature reports whether rel is a feature package (vendor adapters are exempt).
func isFeature(rel string) bool {
	return withinAny(rel, featurePrefixes) && !withinAny(rel, vendorExempt)
}

// rel strips the module prefix so external paths are left untouched.
func rel(p string) string { return strings.TrimPrefix(p, modulePath) }

// forbiddenImports returns the imports pkgPath must not use under the rules
// feature-no-infra, handler-thin, adapter-no-handler, cgo-libs-only-in-audio
// and audio-only-realtime-graph.
func forbiddenImports(pkgPath string, imports []string) []string {
	pkg := rel(pkgPath)
	var bad []string
	for _, imp := range imports {
		if violates(pkg, imp) {
			bad = append(bad, imp)
		}
	}
	return bad
}

func violates(pkg, imp string) bool {
	r := rel(imp)
	internal := r != imp
	hit := func(list []string) bool {
		if internal {
			return false
		}
		for _, d := range list {
			if imp == d || strings.HasPrefix(imp, d+"/") {
				return true
			}
		}
		return false
	}
	hitInternal := func(list []string) bool { return internal && withinAny(r, list) }

	if isFeature(pkg) && (hit(featureDenyExternal) || hitInternal(featureDenyInternal)) {
		return true
	}
	if withinAny(pkg, handlers) && (hit(handlerDenyExternal) || hitInternal(handlerDenyInternal)) {
		return true
	}
	if withinAny(pkg, adapters) && hitInternal(adapterDenyInternal) {
		return true
	}
	if !within(pkg, audioPkg) && hit(cgoLibs) {
		return true
	}
	if internal && within(r, audioPkg) && !within(pkg, audioPkg) && !withinAny(pkg, audioAllowed) {
		return true
	}
	return false
}
