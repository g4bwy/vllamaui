package builtin

import (
	"encoding/json"
	"strconv"

	"llama-webui/server/internal/contracts"
)

// Parameter reads follow json_value() in server-common.h: a missing key, a
// null, or a value of the wrong type falls back to the default. Required
// strings are read with params.at() in the C++, which throws, so a bad one
// here reports the same message text llama-server puts in its 400 body.

func reqString(params map[string]any, key string) (string, string) {
	v, ok := params[key]
	if !ok {
		return "", keyNotFound(key)
	}
	s, ok := v.(string)
	if !ok {
		return "", wrongType("string", v)
	}
	return s, ""
}

func optString(params map[string]any, key, def string) string {
	if s, ok := params[key].(string); ok {
		return s
	}
	return def
}

func optBool(params map[string]any, key string, def bool) bool {
	if b, ok := params[key].(bool); ok {
		return b
	}
	return def
}

// optInt accepts any JSON number and truncates it, as nlohmann does. Any other
// type makes the C++ throw and land on the default, so it does here too.
func optInt(params map[string]any, key string, def int) int {
	switch v := params[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case json.Number:
		if i, err := strconv.Atoi(v.String()); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil {
			return int(f)
		}
	}
	return def
}

func keyNotFound(key string) string { return "key '" + key + "' not found" }

// wrongType keeps the nlohmann wording, which does not name the key.
func wrongType(want string, v any) string {
	return "type must be " + want + ", but is " + typeName(v)
}

// typeName names a JSON value the way nlohmann does in its error messages.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64, float32, int, int64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func fail(msg string) contracts.Result { return contracts.Result{Error: msg} }
