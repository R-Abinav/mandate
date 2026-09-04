package gateway

import "encoding/json"

// extractTopLevelAmount parses body as JSON and reads an int64 amount from a
// top-level field. Razorpay JSON numbers decode as float64; the conversion
// to int64 is exact for paise-denominated amounts within any realistic
// range. Returns found=false on any parse failure or type mismatch — never
// panics on a malformed or unexpected body shape.
func extractTopLevelAmount(body []byte, field string) (amount int64, found bool) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}
	raw, ok := parsed[field]
	if !ok {
		return 0, false
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// extractNestedAmount is extractTopLevelAmount for a field nested one level
// deep, e.g. body["subscription_registration"]["max_amount"]. Every step
// uses a checked type assertion; a missing or malformed parent object
// returns found=false rather than panicking.
func extractNestedAmount(body []byte, parentField, childField string) (amount int64, found bool) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}
	parentRaw, ok := parsed[parentField]
	if !ok {
		return 0, false
	}
	parent, ok := parentRaw.(map[string]interface{})
	if !ok {
		return 0, false
	}
	raw, ok := parent[childField]
	if !ok {
		return 0, false
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// extractNestedString reads a string field nested one level deep, e.g.
// body["notes"]["mandate_request_id"]. Every step uses a checked type
// assertion; a missing or malformed parent object, or a non-string value,
// returns found=false rather than panicking.
func extractNestedString(body []byte, parentField, childField string) (value string, found bool) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", false
	}
	parentRaw, ok := parsed[parentField]
	if !ok {
		return "", false
	}
	parent, ok := parentRaw.(map[string]interface{})
	if !ok {
		return "", false
	}
	raw, ok := parent[childField]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	return s, true
}
