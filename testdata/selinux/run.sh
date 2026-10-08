#!/bin/sh
# Runs check.sh on a throwaway Rocky virtual machine, inside the container
# of the Dockerfile beside it (make selinux-<distro>): the cloud image on
# /images/base.qcow2, dist/ on /pkgs, this directory on /stand, the output
# in /work: check.log for selinux_test.go, console.log of the machine. The
# overlay disk stays in /work after the run, for a look at a failure.
set -u

work=/work
port=2222
ssh_opts="-F /dev/null -i $work/key -p $port -o BatchMode=yes -o ConnectTimeout=5"
ssh_opts="$ssh_opts -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

fail() {
	echo "selinux: $*" >&2
	exit 1
}

guest() {
	# The command is a string for the shell of the guest.
	# shellcheck disable=SC2086,SC2029
	ssh $ssh_opts root@127.0.0.1 "$@"
}

# Files written as root are handed to the user that ran make.
trap '[ -n "${OWNER:-}" ] && chown -R "$OWNER" "$work"' EXIT

[ -r /dev/kvm ] && [ -w /dev/kvm ] ||
	fail "no usable /dev/kvm in the container (rootless docker or userns-remap?)"
set -- /pkgs/mailcrier_*_linux_amd64.rpm
[ -f "$1" ] || fail "no amd64 rpm in dist/"
rpm=$1

rm -f "$work"/key "$work"/key.pub "$work"/user-data "$work"/seed.iso \
	"$work"/disk.qcow2 "$work"/smart.img "$work"/console.log "$work"/check.log
ssh-keygen -q -t ed25519 -N '' -C selinux -f "$work/key" || fail "ssh-keygen failed"
{ cat /stand/user-data; printf '  - %s\n' "$(cat "$work/key.pub")"; } > "$work/user-data"
genisoimage -quiet -output "$work/seed.iso" -volid cidata -joliet -rock \
	"$work/user-data" /stand/meta-data || fail "genisoimage failed"
qemu-img create -q -f qcow2 -F qcow2 -b /images/base.qcow2 "$work/disk.qcow2" || fail "qemu-img failed"
# An IDE disk: QEMU answers SMART commands on it, smartd of step 9 needs one.
qemu-img create -q -f raw "$work/smart.img" 64M || fail "qemu-img failed"

# -no-reboot and panic=exit-failure turn a reboot or a panic of the guest
# into an exit of qemu. SeaBIOS boots the first hard disk only, which is the
# IDE one without bootindex.
qemu-system-x86_64 -machine pc -enable-kvm -cpu host -m 2G -smp 2 \
	-no-reboot -action panic=exit-failure -display none -monitor none \
	-serial "file:$work/console.log" \
	-drive "file=$work/disk.qcow2,format=qcow2,if=none,id=root" \
	-device virtio-blk-pci,drive=root,bootindex=0 \
	-drive "file=$work/seed.iso,format=raw,if=virtio,readonly=on" \
	-drive "file=$work/smart.img,format=raw,if=none,id=smart" \
	-device ide-hd,drive=smart,bus=ide.0,serial=SELINUX0001 \
	-nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$port-:22" &
qemu=$!

# sshd answers once the guest is up and cloud-init gave root the key; the
# loop also ends when qemu exits.
deadline=$(($(date +%s) + 300))
until guest true 2>/dev/null; do
	if ! kill -0 "$qemu" 2>/dev/null; then
		echo "--- console.log" >&2
		tail -50 "$work/console.log" >&2
		fail "VM did not boot: qemu exited"
	fi
	if [ "$(date +%s)" -ge "$deadline" ]; then
		kill "$qemu"
		echo "--- console.log" >&2
		tail -50 "$work/console.log" >&2
		fail "sshd not reachable within 5 minutes"
	fi
	sleep 1
done
guest cloud-init status --wait >/dev/null

name=$(basename "$rpm")
# shellcheck disable=SC2094
for file in /stand/check.sh /stand/receiver.py "$rpm"; do
	guest "cat > /root/$(basename "$file")" < "$file" || fail "copy of $file failed"
done
timeout 30m sh -c "ssh $ssh_opts root@127.0.0.1 sh /root/check.sh /root/$name" > "$work/check.log" 2>&1
status=$?
echo "check.sh exited $status, output in check.log" >&2

guest poweroff 2>/dev/null
i=0
while kill -0 "$qemu" 2>/dev/null; do
	i=$((i + 1))
	if [ $i -ge 120 ]; then
		kill "$qemu"
		break
	fi
	sleep 1
done
wait "$qemu" 2>/dev/null
exit "$status"
