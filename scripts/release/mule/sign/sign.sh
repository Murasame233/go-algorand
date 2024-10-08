#!/usr/bin/env bash
# shellcheck disable=2035,2129

set -exo pipefail
shopt -s nullglob

echo
date "+build_release begin SIGN stage %Y%m%d_%H%M%S"
echo

CHANNEL=${CHANNEL:-$(./scripts/release/mule/common/get_channel.sh "$NETWORK")}
VERSION=${VERSION:-$(./scripts/compute_build_number.sh -f)}
PKG_DIR="./tmp/node_pkgs"
SIGNING_KEY_ADDR=dev@algorand.com
OS_TYPES=(linux darwin)
ARCHS=(amd64 arm64 universal)
ARCH_BITS=(x86_64 aarch64)
# Note that we don't want to use $GNUPGHOME here because that is a documented env var for the gnupg
# project and if it's set in the environment mule will automatically pick it up, which could have
# unintended consequences and be hard to debug.
#
# By naming it something other than $GNUPGHOME, it's essentially acting as an opt-in.
GPG_DIR=${GPG_DIR:-/root/.gnupg}

if ./scripts/release/mule/common/running_in_docker.sh
then
    # It seems that copying/mounting the gpg dir from another machine can result in insecure
    # access privileges, so set the correct permissions to avoid the following warning:
    #
    #   gpg: WARNING: unsafe permissions on homedir '/root/.gnupg'
    #
    find "$GPG_DIR" -type d -exec chmod 700 {} \;
    find "$GPG_DIR" -type f -exec chmod 600 {} \;
fi

pushd /root
cat << EOF > .rpmmacros
%_gpg_name Algorand RPM <rpm@algorand.com>
%__gpg /usr/bin/gpg2
%__gpg_check_password_cmd true
EOF
popd

# Note that when downloading from the cloud that we'll get all packages for all architectures.
if [ -n "$S3_SOURCE" ]
then
    i=0
    for os in "${OS_TYPES[@]}"; do
        for arch in "${ARCHS[@]}"; do
            mkdir -p "$PKG_DIR/$OS_TYPE/$arch"
            arch_bit="${ARCH_BITS[$i]}"
            (
                cd "$PKG_DIR"
                # Note the underscore after ${arch}!
                # Recall that rpm packages have the arch bit in the filenames (i.e., "x86_64" rather than "amd64").
                # Also, the order of the includes/excludes is important!
                aws s3 cp --recursive --exclude "*" --include "*${arch}_*" --include "*$arch_bit.rpm" --exclude "*.sig" --exclude "*.asc" --exclude "*.asc.gz" "s3://$S3_SOURCE/$CHANNEL/$VERSION" .
            )
            i=$((i + 1))
        done
    done
fi

cd "$PKG_DIR"

# TODO: "$PKG_TYPE" == "source"

# https://unix.stackexchange.com/a/46259
# Grab the directories directly underneath (max-depth 1) ./tmp/node_pkgs/ into a space-delimited string.
# This will help us target `linux`, `darwin` and (possibly) `windows` build assets.
# Note the surrounding parens turns the string created by `find` into an array.
for os in "${OS_TYPES[@]}"; do
    for arch in "${ARCHS[@]}"; do
        if [ -d "$os/$arch" ]
        then
            # Only do the subsequent operations in a subshell if the directory is not empty.
            if stat -t "$os/$arch/"* > /dev/null 2>&1
            then
            (
                cd "$os/$arch"

                # Clean package directory of any previous operations.
                rm -rf hashes* *.sig *.asc *.asc.gz

                for file in *.tar.gz *.deb
                do
                    gpg -u "$SIGNING_KEY_ADDR" --detach-sign "$file"
                done

                for file in *.rpm
                do
                    rpmsign --addsign "$file"
                    gpg -u rpm@algorand.com --detach-sign "$file"
                done

                HASHFILE="hashes_${CHANNEL}_${os}_${arch}_${VERSION}"
                md5sum *.tar.gz *.deb *.rpm >> "$HASHFILE"
                sha256sum *.tar.gz *.deb *.rpm >> "$HASHFILE"
                sha512sum *.tar.gz *.deb *.rpm >> "$HASHFILE"

                gpg -u "$SIGNING_KEY_ADDR" --detach-sign "$HASHFILE"
                gpg -u "$SIGNING_KEY_ADDR" --clearsign "$HASHFILE"

                STATUSFILE="build_status_${CHANNEL}_${os}-${arch}_${VERSION}"
                if [[ -f "$STATUSFILE" ]]; then
                    gpg -u "$SIGNING_KEY_ADDR" --clearsign "$STATUSFILE"
                    gzip -c "$STATUSFILE.asc" > "$STATUSFILE.asc.gz"
                fi
            )
            fi
        fi
    done
done

echo
date "+build_release end SIGN stage %Y%m%d_%H%M%S"
echo

