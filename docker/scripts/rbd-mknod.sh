#!/bin/sh
##############################################################################
# rbd-mknod.sh — create the device node for a mapped rbd image.
#
# Called from /etc/udev/rules.d/10-multiprox-rbd.rules as:
#
#   rbd-mknod <kernel-name> <major> <minor>      e.g.  rbd-mknod rbd0 251 0
#
# On a normal host this script would be pointless: /dev is devtmpfs and the
# kernel creates the node itself. A container's /dev is a plain tmpfs, so
# nothing does, and `rbd map` fails after successfully mapping the image:
#
#   rbd: mapping succeeded but /dev/rbd0 is not accessible, is host /dev mounted?
#
# Kept deliberately small and quiet — it runs from a udev rule, where a hanging
# or noisy RUN program stalls event processing for every device.
##############################################################################

[ -n "$1" ] || exit 0

# Only rbd devices. The rule already filters, but this script must never be
# talked into creating a node for something else.
case "$1" in
    rbd[0-9]*) ;;
    *) exit 0 ;;
esac

[ -b "/dev/$1" ] && exit 0

mknod "/dev/$1" b "$2" "$3" 2>/dev/null || exit 0
chgrp disk "/dev/$1" 2>/dev/null
chmod 0660 "/dev/$1" 2>/dev/null

exit 0
