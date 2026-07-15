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
