//go:build windows

package fsperm

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// restrict replaces the object's DACL with one granting only the current user,
// and marks it protected so the parent's entries stop applying.
//
// PROTECTED_DACL_SECURITY_INFORMATION is the half that matters. Adding an ACE
// for the current user changes nothing on its own -- the inherited ACEs that
// grant other groups access remain, and it is those that expose the credentials.
// Protecting the DACL is what severs inheritance.
func restrict(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}

	// Directories propagate the new DACL to what is created inside them, which
	// is the point for auths/ and logs/: files written later inherit the
	// restriction instead of the parent's original entries. Files take no
	// inheritance flags.
	inheritance := uint32(windows.NO_INHERITANCE)
	if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}

	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}

	// nil old ACL: build the list from scratch rather than merging, so nothing
	// pre-existing survives.
	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return fmt.Errorf(i18n.T("构造访问控制列表失败: %w", "building the access control list failed: %w"), err)
	}

	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
	if err != nil {
		return fmt.Errorf(i18n.T("设置 %s 的访问控制失败: %w", "setting access control on %s failed: %w"), path, err)
	}
	return nil
}

// currentUserSID returns the SID this process runs as.
func currentUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf(i18n.T("无法获取当前用户身份: %w", "cannot determine the current user identity: %w"), err)
	}
	return user.User.Sid, nil
}
