//go:build windows

package platform

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// currentUserSID returns the SID of the account this process runs as.
//
// The token user is read fresh on every call rather than cached: caching it
// would keep a stale answer if the process were ever re-tokenised, and the cost
// is a single local call.
func currentUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read own token user: %w", err)
	}
	return user.User.Sid, nil
}

// processUserSID returns the SID of the account a given process runs as.
// Every failure is an error, never a permissive default: a caller uses this to
// decide whether a peer may be trusted, so "unknown" must read as "reject".
func processUserSID(pid uint32) (*windows.SID, error) {
	if pid == 0 {
		return nil, fmt.Errorf("pid 0 is not a real process")
	}
	// PROCESS_QUERY_LIMITED_INFORMATION is the least right that still opens the
	// token. PROCESS_QUERY_INFORMATION would also work and is strictly wider.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return nil, fmt.Errorf("open token of process %d: %w", pid, err)
	}
	defer func() { _ = token.Close() }()

	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read token user of process %d: %w", pid, err)
	}
	return user.User.Sid, nil
}

// sameUserAsSelf reports whether pid runs under this process's account.
// It returns an error rather than false on failure so the caller can say why it
// refused; either outcome must deny.
func sameUserAsSelf(pid uint32) error {
	self, err := currentUserSID()
	if err != nil {
		return err
	}
	peer, err := processUserSID(pid)
	if err != nil {
		return err
	}
	if !windows.EqualSid(peer, self) {
		return fmt.Errorf("peer process %d runs as %s, not %s", pid, peer.String(), self.String())
	}
	return nil
}

// ownerOnlySDDL builds the security descriptor string for an object only the
// current user may touch: D: is the DACL, P marks it protected so no inherited
// ACE from a parent directory can widen it, and the single ACE grants GENERIC_ALL
// to this account's own SID.
//
// The SID is read from our own token, never hardcoded. A well-known SID such as
// S-1-5-32-545 (Users) or a literal "OW"/"CO" would grant the wrong set on a
// domain-joined or multi-user machine.
func ownerOnlySDDL(sid *windows.SID) string {
	return "D:P(A;;GA;;;" + sid.String() + ")"
}

// ownerOnlySecurityAttributes builds SECURITY_ATTRIBUTES granting this account
// alone. It is used for both the runtime/state directories and the control pipe.
//
// The returned value keeps the descriptor reachable through its own field, so
// the caller holding the SECURITY_ATTRIBUTES is enough to keep it alive.
func ownerOnlySecurityAttributes() (*windows.SecurityAttributes, *windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(ownerOnlySDDL(sid))
	if err != nil {
		return nil, nil, fmt.Errorf("build owner-only security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, sd, nil
}
