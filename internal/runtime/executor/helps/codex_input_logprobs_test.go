package helps

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeCodexInputLogprobsReplacesNullWithEmptyArray(t *testing.T) {
	// Captured verbatim from a live ChatGPT backend 400 (input[7] of a real
	// Codex session replay): the backend's own prior "message" output carries
	// "logprobs": null on an "output_text" content part, and the backend
	// rejects that same null when it comes back as input on the next turn.
	body := []byte(`{"input":[` +
		`{"type":"message","status":"completed","role":"assistant","content":[` +
		`{"type":"output_text","text":"<thinking>\n**Inspecting engine/package directory**\n\n\n</thinking>","annotations":[],"logprobs":null}` +
		`]}` +
		`]}`)

	got := SanitizeCodexInputLogprobs(body)

	logprobs := gjson.GetBytes(got, "input.0.content.0.logprobs")
	if !logprobs.IsArray() {
		t.Fatalf("input.0.content.0.logprobs = %s, want []", logprobs.Raw)
	}
	if len(logprobs.Array()) != 0 {
		t.Fatalf("input.0.content.0.logprobs = %s, want empty array", logprobs.Raw)
	}
	// Everything else on the item must be untouched.
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text == "" {
		t.Fatalf("text was dropped: %s", got)
	}
	if status := gjson.GetBytes(got, "input.0.status").String(); status != "completed" {
		t.Fatalf("status changed: %q", status)
	}
}

func TestSanitizeCodexInputLogprobsHandlesMultipleContentParts(t *testing.T) {
	body := []byte(`{"input":[` +
		`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"a","logprobs":null},` +
		`{"type":"output_text","text":"b","logprobs":[]},` +
		`{"type":"output_text","text":"c"}` +
		`]}` +
		`]}`)

	got := SanitizeCodexInputLogprobs(body)

	for i, want := range []string{"a", "b", "c"} {
		path := fmt.Sprintf("input.0.content.%d", i)
		if text := gjson.GetBytes(got, path+".text").String(); text != want {
			t.Fatalf("content.%d.text = %q, want %q", i, text, want)
		}
		lp := gjson.GetBytes(got, path+".logprobs")
		if !lp.IsArray() {
			t.Fatalf("content.%d.logprobs = %s, want an array", i, lp.Raw)
		}
	}
}

func TestSanitizeCodexInputLogprobsLeavesNonNullLogprobsUntouched(t *testing.T) {
	body := []byte(`{"input":[` +
		`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"a","logprobs":[{"token":"a","logprob":-0.1}]}` +
		`]}` +
		`]}`)

	got := SanitizeCodexInputLogprobs(body)
	if string(got) != string(body) {
		t.Fatalf("non-null logprobs payload changed: got=%s want=%s", got, body)
	}
}

func TestSanitizeCodexInputLogprobsIgnoresNonMessageItemsAndMissingInput(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"input":{"id":"item-1"}}`),
		[]byte(`{"input":[{"type":"reasoning","id":"rs-1"}]}`),
		[]byte(`{"input":[{"type":"message","role":"user","content":"plain string content"}]}`),
		[]byte(`{"input":[{"type":"function_call","id":"fc-1"}]}`),
	} {
		if got := string(SanitizeCodexInputLogprobs(body)); got != string(body) {
			t.Fatalf("payload changed: got=%q want=%q", got, body)
		}
	}
}

func TestSanitizeCodexInputLogprobsIsIdempotent(t *testing.T) {
	body := []byte(`{"input":[` +
		`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"a","logprobs":null},` +
		`{"type":"output_text","text":"b"}` +
		`]}` +
		`]}`)

	first := SanitizeCodexInputLogprobs(body)
	second := SanitizeCodexInputLogprobs(first)
	if string(first) != string(second) {
		t.Fatalf("sanitization is not idempotent: first=%s second=%s", first, second)
	}
}
