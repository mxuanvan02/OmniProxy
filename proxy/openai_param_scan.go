package proxy

import "encoding/json"

// scanTopLevelParams extracts the raw JSON value of every top-level object key
// WITHOUT parsing nested values.
//
// Why not json.Unmarshal into map[string]json.RawMessage: that re-parses the
// whole body a second time, including the messages array and the tool schemas —
// which on an agent request are the bulk of the bytes. Measured on a 50 KB body
// that second parse cost ~223 µs and 58 KB of allocation per request, i.e. a 34%
// increase in decode cost for parameters that are usually a handful of small
// scalars. This scanner walks the bytes once, tracking only object/array depth
// and string state, so a nested value is skipped rather than validated.
//
// The returned RawMessages are slices of the input buffer (no copy), valid as
// long as the caller keeps data alive — it does, for the life of the request.
//
// Returns nil when data is not a top-level JSON object or carries no keys, so
// the common "nothing to forward" path allocates nothing.
func scanTopLevelParams(data []byte) map[string]json.RawMessage {
	i := 0
	n := len(data)

	// Skip leading whitespace and require a top-level object.
	for i < n && isJSONSpace(data[i]) {
		i++
	}
	if i >= n || data[i] != '{' {
		return nil
	}
	i++

	var out map[string]json.RawMessage

	for i < n {
		// Between members: whitespace, commas, or the closing brace.
		for i < n && (isJSONSpace(data[i]) || data[i] == ',') {
			i++
		}
		if i >= n {
			return out
		}
		if data[i] == '}' {
			return out
		}
		if data[i] != '"' {
			// Not a key string: the body is malformed. Stop rather than guess.
			return out
		}

		keyStart := i
		keyEnd, ok := skipJSONString(data, i)
		if !ok {
			return out
		}
		i = keyEnd

		// Expect ':' after the key.
		for i < n && isJSONSpace(data[i]) {
			i++
		}
		if i >= n || data[i] != ':' {
			return out
		}
		i++
		for i < n && isJSONSpace(data[i]) {
			i++
		}
		if i >= n {
			return out
		}

		// The value spans [valueStart, valueEnd) and is skipped by depth/string
		// tracking rather than parsed.
		valueStart := i
		valueEnd, ok := skipJSONValue(data, i)
		if !ok {
			return out
		}
		i = valueEnd

		// The key literal in the source includes its quotes, so it can be used
		// directly as a RawMessage without re-encoding.
		keyRaw := json.RawMessage(data[keyStart:keyEnd])
		keyName, err := jsonKeyString(keyRaw)
		if err != nil {
			continue
		}
		if openAIParamDeclared[keyName] || openAIParamBlocked[keyName] {
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage, 8)
		}
		out[keyName] = json.RawMessage(data[valueStart:valueEnd])
	}
	return out
}

// jsonKeyString decodes a quoted JSON key literal into its Go string, applying
// escape processing so a key written as "a\"b" matches its real name.
func jsonKeyString(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// skipJSONString advances past a quoted string starting at i (data[i] must be
// '"'), honouring backslash escapes so a quote inside a string does not end it.
// Returns the index just past the closing quote.
func skipJSONString(data []byte, i int) (int, bool) {
	if i >= len(data) || data[i] != '"' {
		return i, false
	}
	i++
	for i < len(data) {
		switch data[i] {
		case '\\':
			i += 2 // skip the escaped character
			continue
		case '"':
			return i + 1, true
		}
		i++
	}
	return i, false
}

// skipJSONValue advances past one complete JSON value starting at i, whether it
// is a scalar, a string, an array or an object. Nested containers are counted,
// not parsed. Returns the index just past the value.
func skipJSONValue(data []byte, i int) (int, bool) {
	n := len(data)
	if i >= n {
		return i, false
	}
	switch data[i] {
	case '"':
		return skipJSONString(data, i)
	case '{', '[':
		open := data[i]
		var closeByte byte
		if open == '{' {
			closeByte = '}'
		} else {
			closeByte = ']'
		}
		depth := 0
		for i < n {
			switch data[i] {
			case '"':
				end, ok := skipJSONString(data, i)
				if !ok {
					return end, false
				}
				i = end
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 && data[i] == closeByte {
					return i + 1, true
				}
				if depth < 0 {
					return i, false
				}
			}
			i++
		}
		return i, false
	default:
		// Scalar: number, true, false, null — runs until a delimiter.
		for i < n && !isJSONSpace(data[i]) && data[i] != ',' && data[i] != '}' && data[i] != ']' {
			i++
		}
		return i, true
	}
}
