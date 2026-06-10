package config

// ToSnakeCase exposes toSnakeCase for testing.
func ToSnakeCase(s string) string {
	return toSnakeCase(s)
}

// ExportDeriveEnvFile exposes deriveEnvFile for testing.
func ExportDeriveEnvFile(path, env string) string {
	return deriveEnvFile(path, env)
}
