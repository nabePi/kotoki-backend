package protocol

import (
	"reflect"
	"sort"
)

var clientTypes = map[string]reflect.Type{
	"session.start":            reflect.TypeFor[SessionStart](),
	"session.resume":           reflect.TypeFor[SessionResume](),
	"session.end":              reflect.TypeFor[SessionEnd](),
	"client.barge_in":          reflect.TypeFor[ClientBargeIn](),
	"client.playback_finished": reflect.TypeFor[ClientPlaybackFinished](),
	"ping":                     reflect.TypeFor[Ping](),
}

var serverTypes = map[string]reflect.Type{
	"session.ready":         reflect.TypeFor[SessionReady](),
	"session.started":       reflect.TypeFor[SessionStarted](),
	"session.snapshot":      reflect.TypeFor[SessionSnapshot](),
	"session.ending":        reflect.TypeFor[SessionEnding](),
	"session.completed":     reflect.TypeFor[SessionCompleted](),
	"session.failed":        reflect.TypeFor[SessionFailed](),
	"speech.started":        reflect.TypeFor[SpeechStarted](),
	"speech.ended":          reflect.TypeFor[SpeechEnded](),
	"turn.started":          reflect.TypeFor[TurnStarted](),
	"stt.partial":           reflect.TypeFor[STTPartial](),
	"stt.final":             reflect.TypeFor[STTFinal](),
	"ai.generation_started": reflect.TypeFor[AIGenerationStarted](),
	"ai.text_delta":         reflect.TypeFor[AITextDelta](),
	"ai.sentence":           reflect.TypeFor[AISentence](),
	"ai.audio_started":      reflect.TypeFor[AIAudioStarted](),
	"ai.audio_completed":    reflect.TypeFor[AIAudioCompleted](),
	"ai.interrupted":        reflect.TypeFor[AIInterrupted](),
	"turn.completed":        reflect.TypeFor[TurnCompleted](),
	"balance.tick":          reflect.TypeFor[BalanceTick](),
	"pong":                  reflect.TypeFor[Pong](),
	"error":                 reflect.TypeFor[ErrorPayload](),
}

// durable: §7.4.3. `error` non-fatal juga durable, tetapi bergantung payload,
// sehingga IsDurable("error") false; pemanggil memutuskan lewat ErrorPayload.Fatal.
var durable = map[string]bool{
	"session.started": true, "session.ending": true, "session.completed": true, "session.failed": true,
	"turn.started": true, "stt.final": true, "ai.generation_started": true, "ai.sentence": true,
	"ai.audio_started": true, "ai.audio_completed": true, "ai.interrupted": true, "turn.completed": true,
}

// IsDurable melaporkan apakah tipe event server disalin ke replay stream.
func IsDurable(typ string) bool { return durable[typ] }

// Registered mengembalikan tipe client dan server yang terdaftar, terurut.
func Registered() (client, server []string) {
	for k := range clientTypes {
		client = append(client, k)
	}
	for k := range serverTypes {
		server = append(server, k)
	}
	sort.Strings(client)
	sort.Strings(server)
	return client, server
}
