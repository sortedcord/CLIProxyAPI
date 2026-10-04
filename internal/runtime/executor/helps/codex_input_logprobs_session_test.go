package helps

import (
	"os"
	"testing"

	"github.com/tidwall/gjson"
)

// TestSanitizeCodexInputLogprobsOnSyntheticSessionPayload replays a synthetic
// multi-turn Codex session shaped after a live request that triggered a real
// 400 from the ChatGPT backend ("Invalid type for 'input[N].content[0].logprobs':
// expected an array of unknown values, but got null instead."): a mix of user
// turns, function_call/function_call_output pairs, reasoning items, and
// completed assistant messages, some with multiple output_text content parts,
// scattered with the null logprobs the backend's own prior responses leave
// behind. It asserts the sanitizer clears every null logprobs occurrence
// without altering anything else about the input array.
func TestSanitizeCodexInputLogprobsOnSyntheticSessionPayload(t *testing.T) {
	body, err := os.ReadFile("testdata_codex_synthetic_session_payload.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	before := gjson.GetBytes(body, "input")
	if !before.IsArray() {
		t.Fatalf("fixture input is not an array")
	}
	beforeItems := before.Array()
	beforeLen := len(beforeItems)

	nullBefore := 0
	for _, item := range beforeItems {
		if item.Get("type").String() != "message" {
			continue
		}
		item.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "output_text" {
				lp := part.Get("logprobs")
				if lp.Exists() && lp.Type == gjson.Null {
					nullBefore++
				}
			}
			return true
		})
	}
	if nullBefore == 0 {
		t.Fatalf("fixture does not reproduce the bug: found 0 null logprobs occurrences")
	}

	got := SanitizeCodexInputLogprobs(body)

	after := gjson.GetBytes(got, "input")
	if !after.IsArray() {
		t.Fatalf("sanitized input is not an array")
	}
	afterItems := after.Array()
	if len(afterItems) != beforeLen {
		t.Fatalf("input length changed: before=%d after=%d", beforeLen, len(afterItems))
	}

	nullAfter := 0
	textBytesBefore := 0
	textBytesAfter := 0
	for i, item := range afterItems {
		if item.Get("type").String() != "message" {
			continue
		}
		beforeItems[i].Get("content").ForEach(func(_, part gjson.Result) bool {
			textBytesBefore += len(part.Get("text").String())
			return true
		})
		item.Get("content").ForEach(func(_, part gjson.Result) bool {
			textBytesAfter += len(part.Get("text").String())
			if part.Get("type").String() != "output_text" {
				return true
			}
			lp := part.Get("logprobs")
			if lp.Exists() && lp.Type == gjson.Null {
				nullAfter++
			} else if lp.Exists() && !lp.IsArray() {
				t.Fatalf("input.%d: logprobs is neither null nor array: %s", i, lp.Raw)
			}
			return true
		})
	}
	if textBytesAfter != textBytesBefore {
		t.Fatalf("content text bytes changed: before=%d after=%d", textBytesBefore, textBytesAfter)
	}
	if nullAfter != 0 {
		t.Fatalf("sanitizer left %d null logprobs occurrences, want 0", nullAfter)
	}

	t.Logf("sanitized %d null logprobs occurrences across %d input items", nullBefore, beforeLen)
}
