package sandbox

import _ "embed"

// Based on moby/profiles commit 3c28324314729dbade8287e868eef6338c42807a,
// seccomp/default.json (Apache-2.0; see seccomp.LICENSE). All default rules are
// retained except the unconditional ioctl allow, replaced with masked-prefix
// rules allowing every low-32-bit request other than FSSETXATTR and SETFLAGS
// (native/compat). High syscall-argument bits cannot bypass the filter.
// These operations let even an unprivileged container root clear project quota
// inheritance or change its project ID. Ordinary file IO and tty ioctls remain.
//
//go:embed seccomp-quota.json
var quotaSeccomp []byte
