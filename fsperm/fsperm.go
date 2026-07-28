// Package fsperm restricts filesystem objects to the current user.
//
// It exists because os.WriteFile(path, body, 0o600) does not do that on
// Windows, which is the platform this project primarily runs on. Go maps the
// permission bits to the read-only attribute there and nothing else: the
// access-control list is inherited from the parent directory, so a file written
// "0600" can be readable by every group that has an inherited ACE on the
// containing folder.
//
// Measured, not assumed. On the development machine every one of
// slimproxy.effective.yaml, auths/, logs/ and cloudflared.pid.json carried an
// inherited ACE granting a local group ReadAndExecute -- over inbound API keys
// and OAuth refresh tokens in plaintext.
//
// The Unix implementation is a chmod, which is not redundant either: the perm
// argument to os.WriteFile and os.MkdirAll applies only when the object is
// created, so a file that already exists keeps whatever mode it had.
package fsperm

// Restrict tightens path so that only the current user can read or write it,
// and stops it inheriting access from its parent.
//
// Best-effort by contract: it returns an error, and callers are expected to
// report rather than abort on it. A proxy that refuses to start because it
// could not tighten a log directory helps nobody, but one that tightens
// nothing and says nothing is how the measurement above happened.
func Restrict(path string) error { return restrict(path) }
