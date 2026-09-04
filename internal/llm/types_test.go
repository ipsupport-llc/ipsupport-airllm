package llm

import (
	"encoding/json"
	"testing"
)

func TestFunctionCallUnmarshalTolerant(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want FunctionCall
	}{
		{"string args", `{"name":"file","arguments":"{\"a\":1}"}`, FunctionCall{Name: "file", Arguments: `{"a":1}`}},
		{"object args", `{"name":"file","arguments":{"a":1}}`, FunctionCall{Name: "file", Arguments: `{"a":1}`}},
		{"array args", `{"name":"file","arguments":[1,2]}`, FunctionCall{Name: "file", Arguments: `[1,2]`}},
		{"null args", `{"name":"file","arguments":null}`, FunctionCall{Name: "file"}},
		{"absent args", `{"name":"file"}`, FunctionCall{Name: "file"}},
	}
	for _, c := range cases {
		var got FunctionCall
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

func TestFunctionCallMarshalStaysString(t *testing.T) {
	b, err := json.Marshal(FunctionCall{Name: "file", Arguments: `{"a":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"file","arguments":"{\"a\":1}"}` {
		t.Errorf("got %s", b)
	}
}

func TestMessageUnmarshalPlainStringContent(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != "user" || m.Content != "hi" || len(m.Images) != 0 {
		t.Errorf("got %+v", m)
	}
}

func TestMessageUnmarshalContentAbsentOrNull(t *testing.T) {
	for _, body := range []string{`{"role":"assistant","tool_calls":[]}`, `{"role":"user","content":null}`} {
		var m Message
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if m.Content != "" || len(m.Images) != 0 {
			t.Errorf("%s: got %+v, want empty Content and no Images", body, m)
		}
	}
}

func TestMessageUnmarshalMultiPartTextAndImage(t *testing.T) {
	body := `{"role":"user","content":[
		{"type":"text","text":"what is in this image?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
	]}`
	var m Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "what is in this image?" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,AAAA" {
		t.Errorf("Images = %+v", m.Images)
	}
}

func TestMessageUnmarshalImageOnlyContent(t *testing.T) {
	body := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}}]}`
	var m Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "" {
		t.Errorf("Content = %q, want empty", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "https://example.com/a.png" || m.Images[0].Detail != "high" {
		t.Errorf("Images = %+v", m.Images)
	}
}

func TestMessageUnmarshalMultipleImages(t *testing.T) {
	body := `{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
		{"type":"text","text":"compare these"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}
	]}`
	var m Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "compare these" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 2 || m.Images[0].URL != "data:image/png;base64,AAAA" || m.Images[1].URL != "data:image/png;base64,BBBB" {
		t.Errorf("Images order/content wrong: %+v", m.Images)
	}
}

func TestMessageUnmarshalFallbackResetsReceiver(t *testing.T) {
	// Simulate encoding/json reusing an existing slice element: decode
	// twice into the SAME Message value. If the fallback branch didn't
	// reset the receiver, the second decode's Images would still carry
	// the first decode's image.
	var m Message
	body1 := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,FIRST"}}]}`
	if err := json.Unmarshal([]byte(body1), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,FIRST" {
		t.Fatalf("first decode: Images = %+v", m.Images)
	}

	body2 := `{"role":"user","content":"just text now, no images"}`
	if err := json.Unmarshal([]byte(body2), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Images) != 0 {
		t.Errorf("second decode (plain string, no images) must not carry over the first decode's Images, got %+v", m.Images)
	}

	body3 := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,SECOND"}}]}`
	if err := json.Unmarshal([]byte(body3), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,SECOND" {
		t.Errorf("third decode: Images = %+v, want exactly one image from THIS decode, not accumulated from the first", m.Images)
	}
}

func TestMessageUnmarshalMalformedContentErrors(t *testing.T) {
	var m Message
	err := json.Unmarshal([]byte(`{"role":"user","content":42}`), &m)
	if err == nil {
		t.Error("expected an error for content that is neither a string nor a parts array")
	}
}

func TestMessageHasNoCustomMarshalJSON(t *testing.T) {
	b, err := json.Marshal(Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"role":"user","content":"hi"}` {
		t.Errorf("got %s — Message must marshal via plain struct-tag reflection, no custom MarshalJSON", b)
	}
}
