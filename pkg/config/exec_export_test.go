package config

// ValidateExecForTest exposes the section validator to the black-box
// config tests (which cannot call unexported methods).
func ValidateExecForTest(c *Config) []string { return c.validateExec() }
