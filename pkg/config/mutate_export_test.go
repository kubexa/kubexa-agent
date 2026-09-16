package config

// ValidateMutateForTest exposes the section validator to the black-box
// config_test package without exporting it from the production surface.
func ValidateMutateForTest(m MutateConfig) []string { return validateMutateConfig(m) }

// ValidateMutateNodeForTest exposes (*Config).validateMutateNode to the
// black-box config_test package without exporting it from the production
// surface.
func ValidateMutateNodeForTest(c *Config) []string { return c.validateMutateNode() }
