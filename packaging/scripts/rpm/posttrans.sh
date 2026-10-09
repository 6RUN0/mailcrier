#!/bin/sh
# Runs once per transaction that installs or upgrades the package, after
# every package of it: policycoreutils installed along with mailcrier is
# there by now, where %post would miss it. Priority 200 is that of the
# modules of packages; a module of the administrator at 400 stays above it.
# restorecon relabels the binary and the spool, entries of an older version
# included. A failure is reported, never fatal: mail still works for
# callers SELinux does not confine.

module=/usr/share/selinux/packages/mailcrier/mailcrier.cil
hint="mailcrier: SELinux module not installed; to install it run: semodule -X 200 -i $module"

if [ -e /etc/selinux/config ]; then
	if ! command -v semodule >/dev/null; then
		echo "$hint"
	elif ! semodule -X 200 -i "$module" >/dev/null 2>&1; then
		echo "$hint"
	elif command -v selinuxenabled >/dev/null && selinuxenabled; then
		restorecon -RF /usr/sbin/mailcrier /var/spool/mailcrier >/dev/null 2>&1 ||
			echo "mailcrier: relabel failed; run: restorecon -RF /usr/sbin/mailcrier /var/spool/mailcrier"
	fi
fi
exit 0
