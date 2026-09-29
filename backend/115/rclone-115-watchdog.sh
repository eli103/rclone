#!/bin/bash
# Watchdog for the rclone 115 FUSE mount.
#
# Why this exists: the FUSE kernel driver has no request timeout.  When the
# backend fails to answer a request the calling process sleeps in
# request_wait_answer() in uninterruptible sleep, cannot be SIGKILLed, and the
# mount stops serving anything else.  systemd cannot see that state either -
# the daemon is still alive, so Restart=on-failure never fires.  A single
# unanswered request can therefore wedge the mount for days.
#
# This watchdog polls the kernel's per-connection "waiting" counter.  When
# requests stay queued over several consecutive checks it aborts the FUSE
# connection (which releases the stuck tasks with EIO) and restarts the unit,
# logging what it saw first.
#
# Install:
#   sudo install -m 755 rclone-115-watchdog.sh      /usr/local/bin/rclone-115-watchdog
#   sudo install -m 644 rclone-115-watchdog.service /etc/systemd/system/
#   sudo install -m 644 rclone-115-watchdog.timer   /etc/systemd/system/
#   sudo systemctl daemon-reload
#   sudo systemctl enable --now rclone-115-watchdog.timer
#
# Tunables (override with Environment= in the .service unit):
#   MOUNTPOINT  mount point to watch          (default /mnt/115)
#   UNIT        systemd unit to restart       (default rclone-115)
#   THRESHOLD   consecutive bad checks        (default 3)
#   STATE       consecutive-failure counter   (default /run/rclone-115-watchdog.count)

set -u

MOUNTPOINT=${MOUNTPOINT:-/mnt/115}
UNIT=${UNIT:-rclone-115}
THRESHOLD=${THRESHOLD:-3}
STATE=${STATE:-/run/rclone-115-watchdog.count}

log() { logger -t rclone-115-watchdog -- "$*"; }

# The FUSE connection id is the minor of the mount's device: /proc/self/mountinfo
# reports "0:44" for /mnt/115, and the kernel exposes that connection as
# /sys/fs/fuse/connections/44/.  Matching on the device instead of assuming an
# id keeps this correct when the mount is recreated (the id is not stable).
dev=$(awk -v mp="${MOUNTPOINT}" '$5 == mp { print $3; exit }' /proc/self/mountinfo)
if [ -z "${dev}" ]; then
	# Not mounted.  Clear the counter so a stale value cannot trip the abort
	# immediately after the next successful mount.
	echo 0 > "${STATE}"
	exit 0
fi

connid=${dev#*:}
waitingfile="/sys/fs/fuse/connections/${connid}/waiting"
if [ ! -r "${waitingfile}" ]; then
	log "cannot read ${waitingfile}, leaving the mount alone"
	exit 0
fi

waiting=$(cat "${waitingfile}" 2>/dev/null || echo 0)
case "${waiting}" in '' | *[!0-9]*) waiting=0 ;; esac

if [ "${waiting}" -le 0 ]; then
	echo 0 > "${STATE}"
	exit 0
fi

count=$(cat "${STATE}" 2>/dev/null || echo 0)
case "${count}" in '' | *[!0-9]*) count=0 ;; esac
count=$((count + 1))
echo "${count}" > "${STATE}"

log "${MOUNTPOINT}: ${waiting} unanswered FUSE request(s), check ${count}/${THRESHOLD}"
if [ "${count}" -lt "${THRESHOLD}" ]; then
	exit 0
fi

# Capture the evidence before the abort clears it.
log "wedged for ${THRESHOLD} checks: aborting FUSE connection ${connid} and restarting ${UNIT}"
log "processes: $(pgrep -a -f 'rclone.* mount' | tr '\n' '; ')"
log "dmesg tail: $(dmesg 2>/dev/null | tail -5 | tr '\n' '|')"

if echo 1 > "/sys/fs/fuse/connections/${connid}/abort" 2>/dev/null; then
	log "aborted FUSE connection ${connid}; stuck tasks were released with EIO"
else
	log "abort of FUSE connection ${connid} failed (connection already gone?)"
fi
echo 0 > "${STATE}"

if systemctl restart "${UNIT}"; then
	log "restarted ${UNIT}"
else
	log "restart of ${UNIT} FAILED - manual attention needed"
	exit 1
fi
