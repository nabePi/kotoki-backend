package conversation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
)

// entry builds a stream entry: seq, and the server_seq of the durable event before it.
func entry(seq, prev int64) rtstate.EventEntry {
	return rtstate.EventEntry{Seq: seq, PrevSeq: prev, Type: "turn.completed",
		Env: []byte(`{"type":"turn.completed","version":1,"server_seq":` + itoa(seq) + `,"payload":{}}`)}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func seqs(es []rtstate.EventEntry) []int64 {
	var out []int64
	for _, e := range es {
		out = append(out, e.Seq)
	}
	return out
}

// TestReplayAlgorithm is the table of §7.10.4. The stream below holds the durable events 3, 5, 6
// and 9 (the seqs between are transient events); prev_seq chains them.
func TestReplayAlgorithm(t *testing.T) {
	stream := []rtstate.EventEntry{entry(3, 0), entry(5, 3), entry(6, 5), entry(9, 6)}
	// the same stream after the head was trimmed (MAXLEN) or lost while Redis was down
	trimmed := []rtstate.EventEntry{entry(6, 5), entry(9, 6)}

	for _, tc := range []struct {
		name        string
		entries     []rtstate.EventEntry
		last, known int64
		want        []int64
		wantMode    string
	}{
		{name: "empty stream and the client has everything", entries: nil, last: 12, known: 12, wantMode: replayComplete},
		{name: "empty stream and the client is behind", entries: nil, last: 7, known: 12, wantMode: replayUnavailable},
		{name: "empty stream and the client is ahead", entries: nil, last: 13, known: 12, wantMode: replayUnavailable},
		{name: "the client claims more than the server issued", entries: stream, last: 10, known: 9, wantMode: replayUnavailable},
		{name: "nothing was missed", entries: stream, last: 9, known: 9, wantMode: replayComplete},
		{name: "the client has the transient events after the last durable one", entries: stream, last: 11, known: 12, wantMode: replayComplete},
		{name: "one durable event missed", entries: stream, last: 8, known: 9, want: []int64{9}, wantMode: replayComplete},
		{name: "last seq is a transient one between two durable events", entries: stream, last: 4, known: 9, want: []int64{5, 6, 9}, wantMode: replayComplete},
		{name: "everything missed, nothing before the stream", entries: stream, last: 0, known: 9, want: []int64{3, 5, 6, 9}, wantMode: replayComplete},
		{name: "the first missed event follows the client's last durable one", entries: trimmed, last: 5, known: 9, want: []int64{6, 9}, wantMode: replayComplete},
		{name: "the first missed event follows a transient one the client has", entries: trimmed, last: 7, known: 9, want: []int64{9}, wantMode: replayComplete},
		{name: "a durable event between the client's and the first missed one is gone", entries: trimmed, last: 3, known: 9, wantMode: replayUnavailable},
		{name: "the whole head is gone", entries: trimmed, last: 0, known: 9, wantMode: replayUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, mode := replay(tc.entries, tc.last, tc.known)
			require.Equal(t, tc.wantMode, mode)
			require.Equal(t, tc.want, seqs(got))
		})
	}
}

// TestWithReplayFlag: the entry goes out with replay:true and every other field as it was.
func TestWithReplayFlag(t *testing.T) {
	in := []byte(`{"type":"turn.completed","version":1,"server_seq":42,"turn_seq":3,"payload":{"status":"FAILED"}}`)
	out, err := withReplayFlag(in)
	require.NoError(t, err)
	var got, orig map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &got))
	require.NoError(t, json.Unmarshal(in, &orig))
	require.JSONEq(t, "true", string(got["replay"]))
	delete(got, "replay")
	for k, v := range orig {
		require.JSONEq(t, string(v), string(got[k]), k)
	}
	require.Len(t, got, len(orig))
	_, err = withReplayFlag([]byte("not json"))
	require.Error(t, err)
}
