package app

import (
	"fmt"

	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/secrets"
)

// CheckDeploySecrets proves that named workload, database-init and verification
// secrets are present or can be generated/derived by this manifest. It reads
// names and emptiness only, never makes a key, writes a version or emits values.
func CheckDeploySecrets(m *manifest.Manifest, store *secrets.Store) error {
	return CheckDeploySecretsFrom(m, store, "")
}

// CheckDeploySecretsFrom also models CopyAll from a preview's parent. Parent
// names overwrite existing preview names, then this manifest derives its own.
func CheckDeploySecretsFrom(m *manifest.Manifest, store *secrets.Store, from string) error {
	available := map[string]string{}
	if store != nil {
		values, _, err := store.LoadCurrent(m.App)
		if err != nil {
			return err
		}
		for name, value := range values {
			if value == "" {
				available[name] = ""
			} else {
				available[name] = "available"
			}
		}
	}
	if from != "" {
		if from == m.App || from == manifest.ReservedApp {
			return fmt.Errorf("%s cannot inherit secrets from %s", m.App, from)
		}
		if store == nil {
			return fmt.Errorf("secret preflight: the source store for %s is unavailable", from)
		}
		parent, _, err := store.LoadCurrent(from)
		if err != nil {
			return err
		}
		for name, value := range parent {
			if secrets.ValidName(name) {
				if value == "" {
					available[name] = ""
				} else {
					available[name] = "inherited"
				}
			}
		}
	}
	if m.PostgresVersion() != "" {
		if available[postgresPasswordName] == "" {
			available[postgresPasswordName] = "generated"
		}
		if m.InjectsDatabaseURL() && available[DatabaseURLName] == "" {
			available[DatabaseURLName] = "generated"
		}
	}
	if m.HasObjects() {
		for _, name := range []string{ObjectsUserName, ObjectsPasswordName} {
			if available[name] == "" {
				available[name] = "generated"
			}
		}
	}
	if m.Secrets != nil {
		for name := range m.Secrets.Generate {
			if _, present := available[name]; !present {
				available[name] = "generated"
			}
		}
		derived, err := manifest.Derive(m.Secrets.Derive, available, map[string]string{"postgres": PostgresContainer(m.App), "objects": ObjectsContainer(m.App)})
		if err != nil {
			return fmt.Errorf("secret preflight: %w", err)
		}
		for name, value := range derived {
			available[name] = value
		}
	}
	if err := checkSecrets(m, available); err != nil {
		return err
	}
	if m.Data != nil && m.Data.Postgres != nil {
		if missing := secrets.Missing(available, m.Data.Postgres.Secrets); len(missing) > 0 {
			return fmt.Errorf("database init secrets not set: %v; set them with bedrock secret set %s NAME", missing, m.App)
		}
	}
	return checkVerificationSecrets(m, available)
}
