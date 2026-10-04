package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SanitizeCodexInputLogprobs repairs a schema inconsistency in the ChatGPT backend
// Codex uses: a completed response's "message" items carry "logprobs": null on
// their "output_text" content parts, but the same backend rejects that null when
// the item is echoed back verbatim as conversation history on a later turn
// ("Invalid type for 'input[N].content[0].logprobs': expected an array of unknown
// values, but got null instead."). Native Codex clients that replay their own
// rollout history hit this once a session accumulates assistant turns, so this
// normalizes any null "logprobs" on "output_text" content parts within "input" to
// an empty array before the request reaches the backend.
func SanitizeCodexInputLogprobs(body []byte) []byte {
	input := util.GetGJSONBytesNoCopy(body, "input")
	if !input.IsArray() {
		return body
	}

	items := input.Array()
	rebuilt := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		raw := item.Raw
		if item.Get("type").String() == "message" {
			if updated, itemChanged := sanitizeCodexMessageContentLogprobs(raw); itemChanged {
				raw = updated
				changed = true
			}
		}
		rebuilt = append(rebuilt, raw)
	}
	if !changed {
		return body
	}

	updated, errSet := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if errSet != nil {
		return body
	}
	return updated
}

// sanitizeCodexMessageContentLogprobs rewrites null "logprobs" to [] on every
// "output_text" part of a single message item's "content" array.
func sanitizeCodexMessageContentLogprobs(raw string) (string, bool) {
	content := gjson.Get(raw, "content")
	if !content.IsArray() {
		return raw, false
	}

	changed := false
	content.ForEach(func(key, part gjson.Result) bool {
		if part.Get("type").String() != "output_text" {
			return true
		}
		logprobs := part.Get("logprobs")
		if logprobs.Exists() && logprobs.Type != gjson.Null {
			return true
		}
		path := "content." + key.String() + ".logprobs"
		next, errSet := sjson.SetRawBytes([]byte(raw), path, []byte("[]"))
		if errSet == nil {
			raw = string(next)
			changed = true
		}
		return true
	})
	return raw, changed
}
