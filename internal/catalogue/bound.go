package catalogue

import "bytes"

// jsonValueBound reads a JSON document without building it, so an oversized one
// fails before encoding/json allocates for it. It is an upper bound on what the
// decoders build and is exact enough that legitimate routing sections do not
// count against the entries. YAML needs no such reading: the parser itself
// charges what it builds against the budget (internal/yaml/limit.go).

// jsonConfigKeys are the top-level keys any JSON parser here reads. encoding/json
// skips every other key of a config without allocating, so their values, such as
// Xray routing and sing-box route rules, are not counted.
var jsonConfigKeys = [][]byte{[]byte("remarks"), []byte("outbounds"), []byte("servers"), []byte("proxies")}

// jsonMaxDepth is encoding/json's nesting limit; deeper documents fail to decode.
const jsonMaxDepth = 10000

// jsonValueBound bounds the values encoding/json may build from data: one per
// value or key outside strings, skipping unread keys of each config object (the
// root object, or each object in a root array). A key that carries an escape is
// counted, since the decoder may match it after unescaping. It reports false for
// a document nested deeper than encoding/json accepts.
func jsonValueBound(data []byte) (int, bool) {
	configDepth := 1
	if len(data) > 0 && data[0] == '[' {
		configDepth = 2
	}
	n := 1
	var stack []byte
	inString, escaped, candidate, keyEscaped := false, false, false, false
	expectKey, skipping := false, false
	var key []byte
	start := 0
	for i, c := range data {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped, keyEscaped = true, true
			case c == '"':
				inString = false
				if candidate {
					key = data[start:i]
				}
			}
			continue
		}
		atConfig := len(stack) == configDepth && stack[len(stack)-1] == '{'
		switch c {
		case '"':
			inString, start = true, i+1
			candidate, keyEscaped = atConfig && expectKey && !skipping, false
			key = nil
		case '{', '[':
			if len(stack) == jsonMaxDepth {
				return n, false
			}
			stack = append(stack, c)
			expectKey = c == '{'
			if !skipping {
				n++
			}
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if atConfig {
				skipping = false
			}
			expectKey = false
		case ',':
			if atConfig {
				skipping = false
			}
			expectKey = len(stack) > 0 && stack[len(stack)-1] == '{'
			if !skipping {
				n++
			}
		case ':':
			if atConfig && key != nil && !keyEscaped {
				skipping = !readKey(key)
			}
			key, expectKey = nil, false
			if !skipping {
				n++
			}
		}
	}
	return n, true
}

// readKey matches the way encoding/json matches a key to a field: Unicode case
// folding, as bytes.EqualFold does.
func readKey(key []byte) bool {
	for _, k := range jsonConfigKeys {
		if bytes.EqualFold(key, k) {
			return true
		}
	}
	return false
}
