#!/bin/sh
# Host root service. Never formats an existing image or a block device.
set -eu
image=/srv/bkcrab-storage/workspaces.ext4
target=/srv/bkcrab-storage/workspaces
mkdir -p /srv/bkcrab-storage "$target"
if ! [ -f "$image" ]; then
    # Reserve actual disk blocks so unrelated services cannot consume the
    # backing capacity later. This creates a NEW, dedicated 40 GiB filesystem.
    [ ! -e "$image" ] || exit 1
    fallocate -l 40G "$image.new"
    mkfs.ext4 -F -O project,quota -E quotatype=prjquota "$image.new"
    mv "$image.new" "$image"
fi
if ! mountpoint -q "$target"; then
    mount -o loop,prjquota,nodev,nosuid "$image" "$target"
fi
findmnt -n -o OPTIONS --target "$target" | grep -q prjquota
test "$(findmnt -n -o FSTYPE --target "$target")" = ext4

# Bound sandbox writable layers, image cache and daemon logs separately from
# user workspaces, so /tmp writes cannot fill the server's root filesystem.
runtime_image=/srv/bkcrab-storage/docker.ext4
runtime_target=/srv/bkcrab-storage/docker
mkdir -p "$runtime_target" "$target/.users"
if ! [ -f "$runtime_image" ]; then
    [ ! -e "$runtime_image" ] || exit 1
    fallocate -l 20G "$runtime_image.new"
    mkfs.ext4 -F "$runtime_image.new"
    mv "$runtime_image.new" "$runtime_image"
fi
if ! mountpoint -q "$runtime_target"; then
    mount -o loop,nodev,nosuid "$runtime_image" "$runtime_target"
fi
test "$(findmnt -n -o FSTYPE --target "$runtime_target")" = ext4
