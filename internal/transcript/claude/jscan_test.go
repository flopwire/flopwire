package claude

import (
	"encoding/json"
	"testing"
)

func TestEachFieldMatchesEncodingJSON(t *testing.T) {
	docs := []string{
		`{}`,
		` { "a" : 1 , "b":"x\"y\\" ,"c":[1,{"d":"}]"}],"e":null,"f":true,"g":-1.5e3 } `,
		`{"s":"\\\\","t":"back\\\"slash","u":"é\n"}`,
		`{"nested":{"a":{"b":[[],{}]}},"z":"end"}`,
	}
	for _, d := range docs {
		var want map[string]json.RawMessage
		if err := json.Unmarshal([]byte(d), &want); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		if err := eachField([]byte(d), func(k, v []byte) { got[string(k)] = string(v) }); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %v", d, got)
		}
		for k, v := range want {
			if got[k] != string(v) {
				t.Errorf("%s: %s = %s, want %s", d, k, got[k], v)
			}
			var s string
			if v[0] == '"' && json.Unmarshal(v, &s) == nil {
				if u, ok := unquote([]byte(got[k])); !ok || u != s {
					t.Errorf("unquote %s = %q, want %q", got[k], u, s)
				}
			}
		}
	}
}

func TestEachFieldRejectsMalformed(t *testing.T) {
	for _, d := range []string{``, `[]`, `{`, `{"a":1`, `{"a":"x}`, `{"a" 1}`, `{"a":1,}`, `{"a":{"b":1}`, `{a:1}`, `{"a":1 "b":2}`} {
		if err := eachField([]byte(d), func(k, v []byte) {}); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
	var n int
	if err := eachElem([]byte(`[1, "a,b", {"c":[2]}, []]`), func([]byte) { n++ }); err != nil || n != 4 {
		t.Fatalf("eachElem n=%d err=%v", n, err)
	}
}
