//go:build !windows && !darwin

package browsercookies

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
)

// openSecretSession opens a session the items' secrets can be unsealed with.
//
// The "plain" algorithm sends the secret as it is, over the session bus, which
// is the same transport the keyring already uses.
func openSecretSession(ctx context.Context, conn *dbus.Conn) (dbus.ObjectPath, error) {
	secretObject := conn.Object(dbusSecretService, dbus.ObjectPath(dbusSecretObject))
	var output dbus.Variant
	var sessionPath dbus.ObjectPath
	call := secretObject.CallWithContext(ctx, "org.freedesktop.Secret.Service.OpenSession",
		0, "plain", dbus.MakeVariant(""))
	if call.Err != nil {
		return "", call.Err
	}
	if err := call.Store(&output, &sessionPath); err != nil {
		return "", err
	}
	if sessionPath == "" {
		return "", fmt.Errorf("the secret service opened no session")
	}
	return sessionPath, nil
}

// platformSecrets reads the items the secret service matches against the
// query's attributes.
//
// The collection answers with the paths of the items whose attributes match,
// and each item's own attributes, label and secret are read from those paths.
func platformSecrets(ctx context.Context, query SecretQuery) ([]Secret, error) {
	if len(query.Attributes) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbusLookupTimeout)
	defer cancel()

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSecretStore, err)
	}
	defer func() { _ = conn.Close() }()

	session, err := openSecretSession(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSecretStore, err)
	}

	collection := conn.Object(dbusSecretService, dbus.ObjectPath(dbusCollectionPath))
	var paths []dbus.ObjectPath
	call := collection.CallWithContext(ctx, "org.freedesktop.Secret.Collection.SearchItems", 0, query.Attributes)
	if call.Err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSecretStore, call.Err)
	}
	if err := call.Store(&paths); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSecretStore, err)
	}

	secrets := make([]Secret, 0, len(paths))
	for _, path := range paths {
		attributes, _ := itemAttributes(ctx, conn, path)
		secrets = append(secrets, Secret{
			Label:      itemLabel(ctx, conn, path),
			Username:   attributes["username"],
			Value:      readItemSecret(ctx, conn, session, path),
			Attributes: attributes,
		})
	}
	return secrets, nil
}

// itemLabel reads an item's label.
//
// The label is free text the user may have changed, which is why nothing is
// matched on it; it is carried along for the caller that wants to prefer one
// item by name.
func itemLabel(ctx context.Context, conn *dbus.Conn, item dbus.ObjectPath) string {
	object := conn.Object(dbusSecretService, item)
	var label string
	call := object.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.Secret.Item", "Label")
	if call.Err != nil {
		return ""
	}
	if err := call.Store(&label); err != nil {
		return ""
	}
	return label
}
