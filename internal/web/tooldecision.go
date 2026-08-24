package web

import (
	"encoding/json"
	"fmt"
	"math"
)

func toolFunction(name string, tools []map[string]any) map[string]any {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if n, _ := f["name"].(string); n == name {
			return f
		}
	}
	return nil
}

func schemaValid(args map[string]any, fn map[string]any) error {
	params, _ := fn["parameters"].(map[string]any)
	if params == nil {
		return nil
	}
	return validateJSONSchema(args, params, "arguments")
}

// validatedFallbackToolCall gives every non-router tool source the same name,
// tool-choice, object-argument, and JSON-schema checks as the private model
// router. Fallback paths must never be a weaker route around schema validation.
func validatedFallbackToolCall(name string, args any, tools []map[string]any, choice any, index int) (detectedToolCall, bool) {
	fn := toolFunction(name, tools)
	arguments, ok := args.(map[string]any)
	if fn == nil || !ok || !toolChoiceAllows(choice, name) || schemaValid(arguments, fn) != nil {
		return detectedToolCall{}, false
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return detectedToolCall{}, false
	}
	return detectedToolCall{ID: callID(name, string(encoded), index), Name: name, Arguments: encoded}, true
}

func appendUniqueToolCall(calls []detectedToolCall, candidate detectedToolCall) []detectedToolCall {
	for _, existing := range calls {
		if existing.Name == candidate.Name && string(existing.Arguments) == string(candidate.Arguments) {
			return calls
		}
	}
	candidate.ID = callID(candidate.Name, string(candidate.Arguments), len(calls))
	return append(calls, candidate)
}

func validateJSONSchema(value any, schema map[string]any, path string) error {
	if enums, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enums {
			a, _ := json.Marshal(value)
			b, _ := json.Marshal(e)
			if string(a) == string(b) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s is not an allowed enum value", path)
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", path)
		}
		if req, ok := schema["required"].([]any); ok {
			for _, raw := range req {
				n, _ := raw.(string)
				if _, yes := m[n]; !yes {
					return fmt.Errorf("missing required argument %s", n)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
			for n := range m {
				if _, yes := props[n]; !yes {
					return fmt.Errorf("%s.%s is not allowed", path, n)
				}
			}
		}
		for n, v := range m {
			if ps, ok := props[n].(map[string]any); ok {
				if err := validateJSONSchema(v, ps, path+"."+n); err != nil {
					return err
				}
			}
		}
	case "array":
		a, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be array", path)
		}
		if item, ok := schema["items"].(map[string]any); ok {
			for i, v := range a {
				if err := validateJSONSchema(v, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be string", path)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s must be number", path)
		}
	case "integer":
		n, ok := value.(float64)
		if !ok || math.Trunc(n) != n {
			return fmt.Errorf("%s must be integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", path)
		}
	}
	return nil
}
