package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestKeepsLeadingDeveloperAsSystem(t *testing.T) {
	raw := []byte(`{"model":"m","input":[
		{"role":"developer","content":[{"type":"input_text","text":"You are an agent."}]},
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"role":"developer","content":[{"type":"input_text","text":"Tools changed."}]}
	]}`)
	out, err := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", raw, false)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(messages), out)
	}
	for i, want := range []string{"system", "user", "user"} {
		if got := messages[i].Get("role").String(); got != want {
			t.Fatalf("messages[%d].role = %q, want %q: %s", i, got, want, out)
		}
	}
	if got := messages[0].Get("content.0.text").String(); got != "You are an agent." {
		t.Fatalf("system text = %q: %s", got, out)
	}
}
