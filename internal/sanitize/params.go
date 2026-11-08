package sanitize

// ClampTemperature ensures temperature is within Google CloudCode accepted [0.0, 2.0]
func ClampTemperature(temp *float64) *float64 {
	if temp == nil {
		return nil
	}
	t := *temp
	if t < 0.0 {
		t = 0.0
	}
	if t > 2.0 {
		t = 2.0
	}
	return &t
}

// ClampMaxTokens ensures maxOutputTokens is positive and does not exceed maximum allowable bounds
func ClampMaxTokens(maxTokens *int, defaultTokens int) int {
	if defaultTokens <= 0 {
		defaultTokens = 65536
	}
	if maxTokens == nil {
		return defaultTokens
	}
	val := *maxTokens
	if val <= 0 {
		return defaultTokens
	}
	// Google CloudCode upper bound is 65536
	if val > 65536 {
		return 65536
	}
	return val
}

// ClampTopP ensures topP is within (0.0, 1.0]
func ClampTopP(topP *float64) *float64 {
	if topP == nil {
		return nil
	}
	p := *topP
	if p <= 0.0 {
		p = 0.01
	}
	if p > 1.0 {
		p = 1.0
	}
	return &p
}
