package types

// CredentialType identifies the kind of credential stored in the credential management system.
type CredentialType = string

const (
	CredAPIKey   CredentialType = "apiKey"
	CredPostgres CredentialType = "postgres"
	CredMySQL    CredentialType = "mysql"
	CredRedis    CredentialType = "redis"
	CredOAuth2   CredentialType = "oauth2"
	CredBasic    CredentialType = "basic"
)

// CredentialDef references a credential stored in the credential management system.
type CredentialDef struct {
	// Name is the credential ID as registered in the credential management system.
	Name string         `json:"name,omitempty"`
	Type CredentialType `json:"type,omitempty"`
}

// APIKey returns a CredentialDef for an API key credential.
func APIKey(name string) CredentialDef {
	return CredentialDef{Name: name, Type: CredAPIKey}
}

// Postgres returns a CredentialDef for a PostgreSQL database credential.
func Postgres(name string) CredentialDef {
	return CredentialDef{Name: name, Type: CredPostgres}
}

// MySQL returns a CredentialDef for a MySQL database credential.
func MySQL(name string) CredentialDef {
	return CredentialDef{Name: name, Type: CredMySQL}
}

// OAuth2 returns a CredentialDef for an OAuth 2.0 credential.
func OAuth2(name string) CredentialDef {
	return CredentialDef{Name: name, Type: CredOAuth2}
}

// Redis returns a CredentialDef for a Redis credential.
func Redis(name string) CredentialDef {
	return CredentialDef{Name: name, Type: CredRedis}
}

// Basic returns a CredentialDef for a basic (username/password) credential.
func Basic(name string) CredentialDef {
	return CredentialDef{Name: name, Type: CredBasic}
}
