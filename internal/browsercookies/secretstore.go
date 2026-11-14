package browsercookies

import (
	"context"
	"errors"
)

// ErrNoSecretStore means the platform's secret store could not be reached.
//
// It is what separates "the store answered and holds no such item" — which is
// an empty result — from "there is no store to ask", which is a fact about the
// machine the caller has to report differently.
var ErrNoSecretStore = errors.New("the platform's secret store could not be reached")

// SecretQuery names an item in the platform's secret store.
//
// Every platform keys its items its own way, so a query carries the name that
// platform needs and the others are ignored:
//
//   - Linux (and the other unices): every pair in Attributes has to match the
//     item's secret-service attributes.
//   - macOS: Server is the kSecAttrServer of an internet password.
//   - Windows: Target is the Credential Manager target name.
//
// A query that names nothing on the platform it runs on finds nothing.
type SecretQuery struct {
	// Attributes are the secret-service attributes to match (Linux).
	Attributes map[string]string
	// Server is the internet-password server to read (macOS).
	Server string
	// Target is the credential target name to read (Windows).
	Target string
}

// Secret is one item out of the platform's secret store.
type Secret struct {
	// Label is the item's label, when the store keeps one.
	Label string
	// Username is the account name the store filed the secret under.
	Username string
	// Value is the secret itself.
	Value string
	// Attributes are the attributes the store keeps for the item.
	Attributes map[string]string
}

// ReadSecrets asks the platform's secret store for the items a query names.
//
// An empty result is a store that answered and holds no such item. A store
// that cannot be reached answers with ErrNoSecretStore, because the caller
// needs to tell "not signed in" apart from "nothing to ask".
func ReadSecrets(ctx context.Context, query SecretQuery) ([]Secret, error) {
	return platformSecrets(ctx, query)
}
