//go:build windows

package browsercookies

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows secret store is the Credential Manager.
//
// A generic credential has a target name, a user name and a blob. The blob is
// whatever the application wrote; the caller decodes it, because only the
// application knows its shape.

// credTypeGeneric is CRED_TYPE_GENERIC.
const credTypeGeneric = 1

var (
	advapi32DLL   = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW = advapi32DLL.NewProc("CredReadW")
	procCredFree  = advapi32DLL.NewProc("CredFree")
)

// credentialW is CREDENTIALW from wincred.h.
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// platformSecrets reads the credential the query names out of the Credential
// Manager.
//
// CredRead is the same call the PowerShell route uses, minus the interpreter:
// it reads under the caller's own privilege, which is the privilege the
// credential was written with.
func platformSecrets(_ context.Context, query SecretQuery) ([]Secret, error) {
	if query.Target == "" {
		return nil, nil
	}
	target, err := windows.UTF16PtrFromString(query.Target)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSecretStore, err)
	}
	// CredRead writes the credential's pointer into this typed variable, so the
	// memory is reached by a normal Go pointer instead of a uintptr arithmetic
	// dance.
	var cred *credentialW
	ok, _, callErr := procCredReadW.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(credTypeGeneric),
		0,
		uintptr(unsafe.Pointer(&cred)),
	)
	if ok == 0 {
		// The store answered: there is no such credential.
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrNoSecretStore, callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))

	if cred == nil || cred.Type != credTypeGeneric {
		return nil, nil
	}
	secret := ""
	if cred.CredentialBlob != nil && cred.CredentialBlobSize > 0 {
		secret = string(unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize))
	}
	return []Secret{{
		Username:   windows.UTF16PtrToString(cred.UserName),
		Value:      secret,
		Attributes: map[string]string{"target": query.Target},
	}}, nil
}
