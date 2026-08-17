package convert

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// mustJSON marshals v, failing the test on error.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %#v: %v", v, err)
	}
	return b
}

// decode parses JSON bytes into a map, failing the test on error.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return m
}

// assertJSONEq asserts got (JSON bytes) equals want (Go value) after both
// are normalized through encoding/json.
func assertJSONEq(t *testing.T, got []byte, want any) {
	t.Helper()
	wantBytes := mustJSON(t, want)
	var gotV, wantV any
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(wantBytes, &wantV); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Fatalf("mismatch:\n got: %s\nwant: %s", got, wantBytes)
	}
}

func TestNormalizeRequestWhitelist(t *testing.T) {
	body := map[string]any{
		"model":                 "deepseek-v3",
		"messages":              []any{},
		"frequency_penalty":     0.5,
		"logit_bias":            map[string]any{"123": 1.0},
		"logprobs":              true,
		"max_completion_tokens": 100.0,
		"max_tokens":            200.0,
		"metadata":              map[string]any{"k": "v"},
		"modalities":            []any{"text"},
		"parallel_tool_calls":   true,
		"presence_penalty":      -0.2,
		"reasoning_effort":      "high",
		"response_format":       map[string]any{"type": "json_object"},
		"seed":                  42.0,
		"service_tier":          "auto",
		"stop":                  []any{"END"},
		"store":                 false,
		"stream_options":        map[string]any{"include_usage": true},
		"temperature":           0.7,
		"tool_choice":           "auto",
		"tools":                 []any{},
		"top_logprobs":          5.0,
		"top_p":                 0.9,
		"user":                  "u-1",
		// not whitelisted — must be dropped
		"stream":        true,
		"n":             2.0,
		"function_call": "auto",
		"bogus":         "x",
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	for _, key := range []string{
		"model", "messages", "frequency_penalty", "logit_bias", "max_completion_tokens",
		"max_tokens", "metadata", "modalities", "parallel_tool_calls", "presence_penalty",
		"reasoning_effort", "response_format", "seed", "service_tier", "stop", "store",
		"stream_options", "temperature", "tool_choice", "tools", "top_logprobs", "top_p", "user",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("whitelisted key %q dropped", key)
		}
	}
	for _, key := range []string{"stream", "n", "function_call", "bogus"} {
		if _, ok := got[key]; ok {
			t.Errorf("non-whitelisted key %q kept", key)
		}
	}

	// Null-valued whitelisted keys are dropped (meaningless upstream).
	out, err = NormalizeRequest([]byte(`{"model":"m","messages":[],"temperature":null}`), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	if _, ok := got["temperature"]; ok {
		t.Error("null-valued whitelisted key kept")
	}
}

func TestNormalizeRequestDeveloperToSystem(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "developer", "content": "be brief"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "system", "content": "sys"},
		},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	msgs := got["messages"].([]any)
	for i, role := range []string{"system", "user", "assistant", "system"} {
		if got := msgs[i].(map[string]any)["role"]; got != role {
			t.Errorf("message %d role = %v, want %q", i, got, role)
		}
	}
}

func TestNormalizeRequestInvalidBody(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   "{oops",
		"not object": `"hello"`,
		"null":       `null`,
		"empty":      ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeRequest([]byte(body), ""); err == nil {
				t.Fatalf("expected error for body %q", body)
			}
		})
	}
}

func TestNormalizeRequestToolSchemas(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   any
	}{
		{
			name: "bare ref resolved against definitions",
			params: map[string]any{
				"type":        "object",
				"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/Args"}},
				"definitions": map[string]any{"Args": map[string]any{"type": "object"}},
			},
			want: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"type": "object"}},
			},
		},
		{
			name: "ref with siblings not resolved, definitions dropped",
			params: map[string]any{
				"$ref":        "#/definitions/Args",
				"description": "d",
				"definitions": map[string]any{"Args": map[string]any{"type": "object"}},
			},
			want: map[string]any{"$ref": "#/definitions/Args", "description": "d"},
		},
		{
			name: "dollar defs ref resolved",
			params: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/Args"}},
				"$defs":      map[string]any{"Args": map[string]any{"type": "integer"}},
			},
			want: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"type": "integer"}},
			},
		},
		{
			name:   "unresolvable ref kept",
			params: map[string]any{"$ref": "#/definitions/Missing"},
			want:   map[string]any{"$ref": "#/definitions/Missing"},
		},
		{
			name: "anyOf with null simplified to plain type",
			params: map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
				},
			},
			want: map[string]any{"type": "string"},
		},
		{
			name: "anyOf with null merged into sibling keys",
			params: map[string]any{
				"title": "t",
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
				},
			},
			want: map[string]any{"title": "t", "type": "string"},
		},
		{
			// Ported behavior (faithful to convert.js): options are recursed
			// BEFORE the null filter, so a const:null option is cleaned to an
			// empty object and kept; only type:"null" options are filtered.
			name: "anyOf with null via const null cleaned to empty object",
			params: map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"const": nil},
				},
			},
			want: map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{}}},
		},
		{
			name: "anyOf multi-option keeps array minus null",
			params: map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
					map[string]any{"type": "integer"},
				},
			},
			want: map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
		},
		{
			name:   "oneOf all null dropped",
			params: map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"const": nil}}},
			want:   map[string]any{},
		},
		{
			name: "bare null literal in anyOf is kept",
			params: map[string]any{
				"anyOf": []any{nil, map[string]any{"type": "string"}},
			},
			want: map[string]any{"anyOf": []any{nil, map[string]any{"type": "string"}}},
		},
		{
			name:   "enum dedupe and null removal",
			params: map[string]any{"enum": []any{"a", "a", 1.0, 1.0, nil, "b"}},
			want:   map[string]any{"enum": []any{"a", 1.0, "b"}},
		},
		{
			name:   "enum all null dropped",
			params: map[string]any{"enum": []any{nil, nil}},
			want:   map[string]any{},
		},
		{
			name:   "type array reduced to first non-null",
			params: map[string]any{"type": []any{"string", "null"}},
			want:   map[string]any{"type": "string"},
		},
		{
			name:   "type array all null dropped",
			params: map[string]any{"type": []any{"null", "null"}},
			want:   map[string]any{},
		},
		{
			name:   "const null dropped",
			params: map[string]any{"const": nil, "type": "string"},
			want:   map[string]any{"type": "string"},
		},
		{
			name:   "nullable dropped",
			params: map[string]any{"nullable": true, "type": "string"},
			want:   map[string]any{"type": "string"},
		},
		{
			name: "nested properties normalized",
			params: map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"type": []any{"string", "null"}}},
			},
			want: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model":    "m",
				"messages": []any{},
				"tools": []any{map[string]any{
					"type":     "function",
					"function": map[string]any{"name": "f", "parameters": tc.params},
				}},
			}
			out, err := NormalizeRequest(mustJSON(t, body), "")
			if err != nil {
				t.Fatalf("NormalizeRequest: %v", err)
			}
			got := decode(t, out)
			tools := got["tools"].([]any)
			fn := tools[0].(map[string]any)["function"].(map[string]any)
			assertJSONEq(t, mustJSON(t, fn["parameters"]), tc.want)
		})
	}
}

func TestNormalizeRequestSchemaDepthCap(t *testing.T) {
	// 20-deep $ref chain; normalization must stop at the depth cap (12
	// levels), leaving a dangling ref instead of resolving to the leaf.
	defs := map[string]any{}
	for i := 0; i < 20; i++ {
		if i == 19 {
			defs[fmt.Sprintf("D%d", i)] = map[string]any{"type": "string"}
		} else {
			defs[fmt.Sprintf("D%d", i)] = map[string]any{"$ref": fmt.Sprintf("#/definitions/D%d", i+1)}
		}
	}
	deep := map[string]any{
		"type":        "object",
		"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/D0"}},
		"definitions": defs,
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": deep},
		}},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"$ref": "#/definitions/D10"}},
	})

	// A short chain resolves fully.
	shortDefs := map[string]any{
		"D0": map[string]any{"$ref": "#/definitions/D1"},
		"D1": map[string]any{"$ref": "#/definitions/D2"},
		"D2": map[string]any{"type": "integer"},
	}
	short := map[string]any{
		"type":        "object",
		"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/D0"}},
		"definitions": shortDefs,
	}
	body["tools"] = []any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "f", "parameters": short},
	}}
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got = decode(t, out)
	fn = got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "integer"}},
	})
}

func TestNormalizeRequestSchemaNodeBudget(t *testing.T) {
	// Shrink the per-request node budget so the pathological case below is
	// small enough to build cheaply; restore for the rest of the package.
	old := maxSchemaNodes
	maxSchemaNodes = 32
	t.Cleanup(func() { maxSchemaNodes = old })

	// Valid output preserved: a small schema (5 nodes) still normalizes
	// fully under the budget.
	shortDefs := map[string]any{
		"D0": map[string]any{"$ref": "#/definitions/D1"},
		"D1": map[string]any{"$ref": "#/definitions/D2"},
		"D2": map[string]any{"type": "integer"},
	}
	short := map[string]any{
		"type":        "object",
		"properties":  map[string]any{"a": map[string]any{"$ref": "#/definitions/D0"}},
		"definitions": shortDefs,
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": short},
		}},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest (small schema): %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "integer"}},
	})

	// Pathological schema: a wide tree far exceeding the budget, with a
	// normalization-triggering key ("nullable", dropped by normalization) on
	// every leaf. The budget must stop normalization partway (early leaves
	// normalized, the vast majority returned unchanged) instead of re-copying
	// the subtree at every ancestor up to maxSchemaDepth.
	wide := map[string]any{
		"type":        "object",
		"definitions": map[string]any{"Unused": map[string]any{"type": "string"}},
		"properties":  map[string]any{},
	}
	for i := 0; i < 100; i++ {
		child := map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
		for j := 0; j < 100; j++ {
			child["properties"].(map[string]any)[fmt.Sprintf("p%d_%d", i, j)] =
				map[string]any{"type": "string", "nullable": true}
		}
		wide["properties"].(map[string]any)[fmt.Sprintf("o%d", i)] = child
	}
	body["tools"] = []any{map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "f", "parameters": wide},
	}}
	out, err = NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest (pathological schema): %v", err)
	}
	got = decode(t, out)
	fn = got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	// Normalization ran at the root (definitions dropped) and the untouched
	// remainder is still structurally intact.
	params := fn["parameters"].(map[string]any)
	if _, ok := params["definitions"]; ok {
		t.Error("root definitions survived normalization")
	}
	props := params["properties"].(map[string]any)
	if len(props) != 100 {
		t.Fatalf("properties = %d entries, want 100 (untouched remainder)", len(props))
	}
	total, normalized := 0, 0
	for _, o := range props {
		child := o.(map[string]any)
		for _, p := range child["properties"].(map[string]any) {
			total++
			if _, dropped := p.(map[string]any)["nullable"]; !dropped {
				normalized++
			}
		}
	}
	if normalized == 0 {
		t.Error("no leaf was normalized before the budget exhausted")
	}
	if normalized == total {
		t.Error("every leaf was normalized — the node budget did not stop early")
	}
}

func TestNormalizeRequestNestedDefinitionsMerge(t *testing.T) {
	// Definitions carried by a mid-tree node are merged into the table and
	// resolvable by deeper refs.
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"wrap": map[string]any{
				"type":        "object",
				"definitions": map[string]any{"Inner": map[string]any{"type": "integer"}},
				"properties":  map[string]any{"y": map[string]any{"$ref": "#/definitions/Inner"}},
			},
		},
	}
	body := map[string]any{
		"model":    "m",
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": params},
		}},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assertJSONEq(t, mustJSON(t, fn["parameters"]), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"wrap": map[string]any{
				"type":       "object",
				"properties": map[string]any{"y": map[string]any{"type": "integer"}},
			},
		},
	})
}

func TestEncodeSSE(t *testing.T) {
	payload := mustJSON(t, map[string]any{"id": "x", "choices": []any{}})
	frame := EncodeSSE(payload)
	want := "data: " + string(payload) + "\n\n"
	if string(frame) != want {
		t.Fatalf("frame = %q, want %q", frame, want)
	}
	if inner := strings.TrimSuffix(strings.TrimPrefix(string(frame), "data: "), "\n\n"); strings.Contains(inner, " ") {
		t.Fatalf("frame payload contains whitespace: %q", frame)
	}
	if !strings.HasSuffix(string(frame), "\n\n") {
		t.Fatalf("frame does not end with \\n\\n: %q", frame)
	}
}

func TestDONE(t *testing.T) {
	if string(DONE) != "data: [DONE]\n\n" {
		t.Fatalf("DONE = %q, want %q", DONE, "data: [DONE]\n\n")
	}
}

func TestSanitizeChunk(t *testing.T) {
	t.Run("drop empty choice chunk", func(t *testing.T) {
		out, drop := SanitizeChunk(mustJSON(t, map[string]any{"id": "x", "choices": []any{}}))
		if !drop || out != nil {
			t.Fatalf("drop = %v, out = %q; want drop with nil out", drop, out)
		}
	})

	t.Run("keep usage-only chunk", func(t *testing.T) {
		out, drop := SanitizeChunk(mustJSON(t, map[string]any{
			"id": "x", "usage": map[string]any{"prompt_tokens": 1.0},
		}))
		if drop {
			t.Fatal("usage-only chunk dropped")
		}
		got := decode(t, out)
		if got["usage"].(map[string]any)["prompt_tokens"] != float64(1) {
			t.Fatalf("usage not preserved: %v", got["usage"])
		}
		if ch := got["choices"].([]any); len(ch) != 0 {
			t.Fatalf("choices = %v, want empty", ch)
		}
	})

	t.Run("defaults id object created model", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		if id, _ := got["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
			t.Fatalf("id = %q, want chatcmpl- prefix", id)
		}
		if got["object"] != "chat.completion.chunk" {
			t.Fatalf("object = %v", got["object"])
		}
		if c, _ := got["created"].(float64); c <= 0 {
			t.Fatalf("created = %v", got["created"])
		}
		if got["model"] != "" {
			t.Fatalf("model = %v, want empty", got["model"])
		}
	})

	t.Run("preserves reasoning_content separate from content", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"","reasoning_content":"think step 1"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if delta["reasoning_content"] != "think step 1" {
			t.Fatalf("reasoning_content = %v", delta["reasoning_content"])
		}
		if delta["content"] != "" {
			t.Fatalf("content = %v, want empty (not merged with reasoning)", delta["content"])
		}
	})

	t.Run("null content removed", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{"content":null,"reasoning_content":"r"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if _, ok := delta["content"]; ok {
			t.Fatalf("null content key kept: %v", delta)
		}
		if delta["reasoning_content"] != "r" {
			t.Fatalf("reasoning_content = %v", delta["reasoning_content"])
		}
	})

	t.Run("data prefix and passthrough fields", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":5,"model":"m","system_fingerprint":"fp","choices":[{"index":1,"delta":{"content":"x"},"finish_reason":"stop","logprobs":{"a":1}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		if got["id"] != "c1" || got["object"] != "chat.completion.chunk" || got["created"] != float64(5) ||
			got["model"] != "m" || got["system_fingerprint"] != "fp" {
			t.Fatalf("passthrough fields mangled: %v", got)
		}
		choice := got["choices"].([]any)[0].(map[string]any)
		if choice["index"] != float64(1) || choice["finish_reason"] != "stop" || choice["logprobs"] == nil {
			t.Fatalf("choice fields mangle: %v", choice)
		}
	})

	t.Run("preserves in-band error chunk with map", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"id":"c1","error":{"message":"quota exceeded","type":"insufficient_quota","code":"quota_exceeded"}}`))
		if drop {
			t.Fatal("error chunk dropped")
		}
		got := decode(t, out)
		if got["id"] != "c1" {
			t.Fatalf("id = %v, want c1", got["id"])
		}
		errObj, ok := got["error"].(map[string]any)
		if !ok {
			t.Fatalf("error object = %v, want map", got["error"])
		}
		if errObj["message"] != "quota exceeded" || errObj["type"] != "insufficient_quota" || errObj["code"] != "quota_exceeded" {
			t.Fatalf("error obj mangled: %v", errObj)
		}
	})

	t.Run("preserves in-band error chunk with string", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"error":"upstream stream interrupted"}`))
		if drop {
			t.Fatal("error chunk with string dropped")
		}
		got := decode(t, out)
		errObj, ok := got["error"].(map[string]any)
		if !ok {
			t.Fatalf("error object = %v, want map", got["error"])
		}
		if errObj["message"] != "upstream stream interrupted" || errObj["type"] != "upstream_error" {
			t.Fatalf("error obj mangled: %v", errObj)
		}
	})

	t.Run("malformed and non-JSON lines dropped", func(t *testing.T) {
		for _, line := range []string{`{bad`, `data: {bad`, `hello`, `data: `, `: keep-alive`, ``} {
			out, drop := SanitizeChunk([]byte(line))
			if !drop || out != nil {
				t.Fatalf("line %q: drop = %v, out = %q; want drop with nil out", line, drop, out)
			}
		}
	})
}

func TestErrorChunk(t *testing.T) {
	frame := ErrorChunk("boom", "E1")
	want := `data: ` + string(mustJSON(t, map[string]any{
		"error": map[string]any{"message": "boom", "type": "upstream_error", "code": "E1"},
	})) + "\n\n"
	if string(frame) != want {
		t.Fatalf("ErrorChunk = %q, want %q", frame, want)
	}
	frame = ErrorChunk("boom", "")
	if strings.Contains(string(frame), "code") {
		t.Fatalf("code key present without code: %q", frame)
	}
	if !strings.HasSuffix(string(frame), "\n\n") {
		t.Fatalf("missing trailing newline: %q", frame)
	}
}

func TestAccumulator(t *testing.T) {
	t.Run("full stream", func(t *testing.T) {
		a := NewAccumulator()
		lines := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":100,"model":"m",` +
				`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel","reasoning_content":"think "}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"step"}}]}`, // reason "think step"
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`{"choices":[{"index":1,"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"g","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
			`{"choices":[{"index":0,"delta":{"content":""}}]}`, // empty content: no finish change
			`{"id":"c1","model":"m","usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8},"system_fingerprint":"fp"}`,
			`data: [DONE]`,
		}
		for _, line := range lines {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add(%q): %v", line, err)
			}
		}
		out := decode(t, a.Finish())
		if out["id"] != "c1" || out["object"] != "chat.completion" || out["model"] != "m" || out["created"] != float64(100) {
			t.Fatalf("header fields: %v", out)
		}
		if out["system_fingerprint"] != "fp" {
			t.Fatalf("system_fingerprint = %v", out["system_fingerprint"])
		}
		choice := out["choices"].([]any)[0].(map[string]any)
		if choice["index"] != float64(0) || choice["finish_reason"] != "tool_calls" {
			t.Fatalf("choice = %v", choice)
		}
		msg := choice["message"].(map[string]any)
		if msg["role"] != "assistant" || msg["content"] != "Hello" {
			t.Fatalf("message = %v", msg)
		}
		if msg["reasoning_content"] != "think step" {
			t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
		}
		calls := msg["tool_calls"].([]any)
		if len(calls) != 2 {
			t.Fatalf("tool_calls = %v", calls)
		}
		first := calls[0].(map[string]any)
		if first["id"] != "call_1" || first["type"] != "function" ||
			first["function"].(map[string]any)["name"] != "f" ||
			first["function"].(map[string]any)["arguments"] != `{"x":1}` {
			t.Fatalf("tool call 0 = %v", first)
		}
		if calls[1].(map[string]any)["id"] != "call_2" {
			t.Fatalf("tool call 1 = %v", calls[1])
		}
		usage := out["usage"].(map[string]any)
		if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(3) || usage["total_tokens"] != float64(8) {
			t.Fatalf("usage = %v", usage)
		}
	})

	t.Run("empty stream zeroed usage", func(t *testing.T) {
		a := NewAccumulator()
		if err := a.Add([]byte("data: [DONE]")); err != nil {
			t.Fatalf("Add: %v", err)
		}
		out := decode(t, a.Finish())
		choice := out["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
		}
		if msg := choice["message"].(map[string]any); msg["content"] != "" {
			t.Fatalf("content = %v", msg["content"])
		}
		usage := out["usage"].(map[string]any)
		for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if usage[k] != float64(0) {
				t.Fatalf("usage[%s] = %v, want 0", k, usage[k])
			}
		}
	})

	t.Run("finish reason last non-empty wins", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"finish_reason":"tool_calls"}]}`,
			`{"choices":[{"index":0,"delta":{"content":"x"}}]}`, // no finish_reason
			`{"choices":[{"index":0,"finish_reason":"stop"}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		if fr := out["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "stop" {
			t.Fatalf("finish_reason = %v, want stop", fr)
		}
	})

	t.Run("default ids when missing", func(t *testing.T) {
		a := NewAccumulator()
		if err := a.Add([]byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)); err != nil {
			t.Fatalf("Add: %v", err)
		}
		out := decode(t, a.Finish())
		if id, _ := out["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
			t.Fatalf("id = %v", out["id"])
		}
		if c, _ := out["created"].(float64); c <= 0 {
			t.Fatalf("created = %v", out["created"])
		}
	})

	t.Run("malformed line errors", func(t *testing.T) {
		a := NewAccumulator()
		if err := a.Add([]byte(`data: {bad`)); err == nil {
			t.Fatal("expected error for malformed chunk")
		}
	})

	t.Run("non-data lines ignored", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{"", ": keep-alive", "event: message", "id: 1", "retry: 100"} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add(%q): %v", line, err)
			}
		}
		out := decode(t, a.Finish())
		if msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any); msg["content"] != "" {
			t.Fatalf("content = %v", msg["content"])
		}
	})

	t.Run("error chunk in stream returns descriptive error", func(t *testing.T) {
		a := NewAccumulator()
		err := a.Add([]byte(`{"error":{"message":"token rate limit reached","type":"rate_limit"}}`))
		if err == nil {
			t.Fatal("expected error for error chunk")
		}
		if !strings.Contains(err.Error(), "token rate limit reached") {
			t.Fatalf("error %v does not contain message", err)
		}

		a2 := NewAccumulator()
		err2 := a2.Add([]byte(`{"error":"context window exceeded"}`))
		if err2 == nil {
			t.Fatal("expected error for string error chunk")
		}
		if !strings.Contains(err2.Error(), "context window exceeded") {
			t.Fatalf("error %v does not contain message", err2)
		}
	})

	t.Run("finish reason defaults to tool_calls when tool calls present without finish_reason", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"calc","arguments":"{}"}}]}}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		if fr := out["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "tool_calls" {
			t.Fatalf("finish_reason = %v, want tool_calls", fr)
		}
	})

	t.Run("finish reason preserves explicit non-empty reason when tool calls present", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"calc","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"finish_reason":"length"}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		if fr := out["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "length" {
			t.Fatalf("finish_reason = %v, want length", fr)
		}
	})
}
func TestNormalizeRequestInjectsEndTurnTool(t *testing.T) {
	body := map[string]any{
		"model":    "deepseek/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "custom_tool",
					"description": "custom user tool",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
	}

	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("want 2 tools (custom_tool + end_turn), got %v", tools)
	}

	hasCustom, hasEndTurn := false, false
	for _, tVal := range tools {
		if tm, ok := tVal.(map[string]any); ok {
			if fn, ok := tm["function"].(map[string]any); ok {
				if fn["name"] == "custom_tool" {
					hasCustom = true
				}
				if fn["name"] == "end_turn" {
					hasEndTurn = true
				}
			}
		}
	}
	if !hasCustom || !hasEndTurn {
		t.Errorf("hasCustom = %v, hasEndTurn = %v", hasCustom, hasEndTurn)
	}
}

func TestExtractReasoningEffort(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "direct reasoning_effort",
			payload: map[string]any{"reasoning_effort": "max"},
			want:    "max",
		},
		{
			name:    "nested reasoning.effort",
			payload: map[string]any{"reasoning": map[string]any{"effort": "high"}},
			want:    "high",
		},
		{
			name:    "nested thinking.type",
			payload: map[string]any{"thinking": map[string]any{"type": "enabled"}},
			want:    "enabled",
		},
		{
			name:    "empty payload",
			payload: nil,
			want:    "",
		},
		{
			name:    "no effort fields",
			payload: map[string]any{"model": "deepseek-v4-flash"},
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractReasoningEffort(tc.payload); got != tc.want {
				t.Errorf("ExtractReasoningEffort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeRequestReasoningEffort(t *testing.T) {
	// Nested reasoning: { "effort": "max" } normalizes to reasoning_effort: "max"
	body := map[string]any{
		"model":     "deepseek/deepseek-v4-flash",
		"messages":  []any{map[string]any{"role": "user", "content": "hello"}},
		"reasoning": map[string]any{"effort": "max"},
	}
	out, err := NormalizeRequest(mustJSON(t, body), "")
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	got := decode(t, out)
	if gotEff, ok := got["reasoning_effort"].(string); !ok || gotEff != "max" {
		t.Errorf("reasoning_effort = %v, want \"max\"", got["reasoning_effort"])
	}
}

// TestSanitizeChunkBranchTable pins the branch-level edges of chunk
// sanitization: a non-string reasoning_content is dropped (only strings
// pass through), a fractional created is truncated to its integer part,
// and a choice carrying an empty delta is KEPT (the choice exists, so the
// chunk is not dropped).
func TestSanitizeChunkBranchTable(t *testing.T) {
	t.Run("non-string reasoning_content dropped", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{"content":"x","reasoning_content":123}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		delta := got["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if _, ok := delta["reasoning_content"]; ok {
			t.Errorf("non-string reasoning_content kept: %v", delta)
		}
		if delta["content"] != "x" {
			t.Errorf("content = %v, want x", delta["content"])
		}
	})

	t.Run("fractional created truncated", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"created":123.7,"choices":[{"delta":{"content":"x"}}]}`))
		if drop {
			t.Fatal("chunk dropped")
		}
		got := decode(t, out)
		if got["created"] != float64(123) {
			t.Errorf("created = %v, want 123 (truncated, not rounded)", got["created"])
		}
	})

	t.Run("empty delta choice kept", func(t *testing.T) {
		out, drop := SanitizeChunk([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
		if drop {
			t.Fatal("choice with an empty delta must be kept")
		}
		got := decode(t, out)
		choice := got["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
		}
		if d := choice["delta"].(map[string]any); len(d) != 0 {
			t.Errorf("delta = %v, want empty", d)
		}
	})
}

// TestAccumulatorLaterFragmentToolCall pins the tool-call stitcher: id,
// type and function name may arrive on a LATER fragment (the first one
// carries only arguments); the accumulated arguments concatenate across
// fragments regardless of which fragment carries the metadata. A gap in
// tool-call indices sorts the assembled output by index (the index itself
// is not part of the output shape).
func TestAccumulatorLaterFragmentToolCall(t *testing.T) {
	t.Run("id and name on a later fragment", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_later","type":"function","function":{"name":"fn_later","arguments":"1}"}}]}}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add(%q): %v", line, err)
			}
		}
		out := decode(t, a.Finish())
		calls := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
		if len(calls) != 1 {
			t.Fatalf("tool_calls = %v, want 1", calls)
		}
		first := calls[0].(map[string]any)
		if first["id"] != "call_later" || first["type"] != "function" {
			t.Errorf("tool call id/type = %v / %v, want call_later / function", first["id"], first["type"])
		}
		if fn := first["function"].(map[string]any); fn["name"] != "fn_later" || fn["arguments"] != `{"a":1}` {
			t.Errorf("tool call function = %v, want name fn_later args %q", fn, `{"a":1}`)
		}
	})

	t.Run("index gap sorted output", func(t *testing.T) {
		a := NewAccumulator()
		for _, line := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"id":"c3","function":{"name":"f3","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"f1","arguments":"{}"}}]}}]}`,
		} {
			if err := a.Add([]byte(line)); err != nil {
				t.Fatalf("Add: %v", err)
			}
		}
		out := decode(t, a.Finish())
		calls := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
		if len(calls) != 2 {
			t.Fatalf("tool_calls = %v, want 2", calls)
		}
		if id := calls[0].(map[string]any)["id"]; id != "c1" {
			t.Errorf("calls[0].id = %v, want c1 (sorted by index)", id)
		}
		if id := calls[1].(map[string]any)["id"]; id != "c3" {
			t.Errorf("calls[1].id = %v, want c3", id)
		}
	})
}

// TestAccumulatorReasoningOnlyFinish pins a reasoning-only stream: the
// Finish response carries the concatenated reasoning_content while content
// stays empty.
func TestAccumulatorReasoningOnlyFinish(t *testing.T) {
	a := NewAccumulator()
	for _, line := range []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"think "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"more"},"finish_reason":"stop"}]}`,
	} {
		if err := a.Add([]byte(line)); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	out := decode(t, a.Finish())
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "" {
		t.Errorf("content = %v, want empty", msg["content"])
	}
	if msg["reasoning_content"] != "think more" {
		t.Errorf("reasoning_content = %v, want 'think more'", msg["reasoning_content"])
	}
}

// TestNormalizeEnumCrossTypeDedupe pins the enum dedupe key: Go type AND
// JSON encoding combine ("%T:repr"), so values with the same JSON
// representation but different Go types (float64(1.0) vs int(1) — both
// "1" after encoding) are BOTH kept, while exact duplicates are removed
// and null entries dropped. (Tested white-box because the request path
// round-trips numbers through encoding/json, which collapses every number
// to float64.)
func TestNormalizeEnumCrossTypeDedupe(t *testing.T) {
	schema := map[string]any{"enum": []any{1.0, 1, "1", "a", "a", true, true, nil}}
	normalizeEnumField(schema)
	enum := schema["enum"].([]any)
	want := []any{1.0, 1, "1", "a", true}
	if len(enum) != len(want) {
		t.Fatalf("enum = %v (%d), want %v (%d): cross-type entries kept, exact dupes removed", enum, len(enum), want, len(want))
	}
	for i := range want {
		if enum[i] != want[i] || fmt.Sprintf("%T", enum[i]) != fmt.Sprintf("%T", want[i]) {
			t.Errorf("enum[%d] = %v (%T), want %v (%T)", i, enum[i], enum[i], want[i], want[i])
		}
	}
}

// TestParseSSEDataWhitespaceEdges pins the SSE data-line parser edges:
// leading whitespace is tolerated (on both "data:" and plain-JSON lines),
// a space between "data" and the colon is NOT a data line (it falls
// through to the plain-JSON check and is skipped), and extra spaces after
// the colon are trimmed.
func TestParseSSEDataWhitespaceEdges(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []byte
		ok   bool
	}{
		{"leading spaces before data", "  data: {\"a\":1}", []byte(`{"a":1}`), true},
		{"space before colon not matched", "data : {\"a\":1}", nil, false},
		{"extra space after colon", "data:  {\"a\":1}", []byte(`{"a":1}`), true},
		{"plain json", `{"a":1}`, []byte(`{"a":1}`), true},
		{"plain json with leading space", `  {"a":1}`, []byte(`{"a":1}`), true},
		{"blank", "", nil, false},
		{"comment", ": hi", nil, false},
		{"event field", "event: message", nil, false},
		{"data with empty payload", "data: ", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSSEData([]byte(tc.line))
			if ok != tc.ok || string(got) != string(tc.want) {
				t.Errorf("parseSSEData(%q) = %q, %v; want %q, %v", tc.line, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestNormalizeRequestNonMapToolsEndTurn pins end_turn injection when the
// tools array holds ONLY non-map entries: each entry is skipped by the
// schema normalizer but left in place, and end_turn is still appended
// (the array is non-empty, so the foreign_toolset validation path
// triggers). An existing end_turn must never be duplicated.
func TestNormalizeRequestNonMapToolsEndTurn(t *testing.T) {
	t.Run("all non-map tools get end_turn appended", func(t *testing.T) {
		body := map[string]any{
			"model":    "m",
			"messages": []any{},
			"tools":    []any{"not-a-map", 42},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		tools, ok := got["tools"].([]any)
		if !ok {
			t.Fatal("tools missing")
		}
		if len(tools) != 3 {
			t.Fatalf("tools = %v, want [not-a-map, 42, end_turn]", tools)
		}
		if tools[0] != "not-a-map" || tools[1] != float64(42) {
			t.Errorf("non-map entries were modified: %v", tools[:2])
		}
		endTurn, ok := tools[2].(map[string]any)
		if !ok || endTurn["function"].(map[string]any)["name"] != "end_turn" {
			t.Errorf("tools[2] = %v, want the end_turn tool", tools[2])
		}
	})

	t.Run("existing end_turn not duplicated", func(t *testing.T) {
		body := map[string]any{
			"model":    "m",
			"messages": []any{},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "end_turn",
					"parameters": map[string]any{"type": "object"},
				},
			}},
		}
		out, err := NormalizeRequest(mustJSON(t, body), "")
		if err != nil {
			t.Fatalf("NormalizeRequest: %v", err)
		}
		got := decode(t, out)
		tools, _ := got["tools"].([]any)
		count := 0
		for _, tVal := range tools {
			if tm, ok := tVal.(map[string]any); ok {
				if fn, ok := tm["function"].(map[string]any); ok && fn["name"] == "end_turn" {
					count++
				}
			}
		}
		if count != 1 {
			t.Errorf("end_turn appears %d times, want exactly 1: %v", count, tools)
		}
	})
}
