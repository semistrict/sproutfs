#!/usr/bin/env bash
# Build the x86_64 guest kernel TestOnlyANestedGuestIsOfferedHardwareVirtualisation
# boots: Firecracker's CI configuration with KVM built in.
#
# The CI kernel is built without KVM, and such a kernel never turns VMX on in
# IA32_FEATURE_CONTROL and clears the vmx flag it was offered (Linux's
# arch/x86/kernel/cpu/feat_ctl.c). A guest of it shows no VMX whether it was
# offered or not, and has no /dev/kvm to try. This one is the same
# configuration with KVM, KVM_INTEL and KVM_AMD built in rather than modules,
# since the guest's root has no modules to load, from the upstream stable
# release the CI kernel is.
#
# Usage: nested-kernel.sh REPO WORK OUTPUT
#
# WORK keeps the source and the build, so a second build there compiles
# nothing. ARCH and CROSS_COMPILE pass through to make, which is how a host
# that is not x86_64 builds it.
set -euo pipefail
repo=$(cd "${1:?repository required}" && pwd)
work=${2:?work directory required}
output=${3:?output path required}
version=6.18.44
sha256=0f72d938f06828e82c90405174fe572287db7bfe089e2fc46572a99a7f240d43
config="$repo/third_party/firecracker/resources/guest_configs/microvm-kernel-ci-x86_64-6.18.config"
source="$work/linux-$version"
build="$work/linux-$version-nested"
options=(VIRTUALIZATION KVM KVM_INTEL KVM_AMD)

mkdir -p "$work" "$build"
if [[ ! -e $source/.sproutfs-unpacked ]]; then
    curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$version.tar.xz" -o "$work/linux-$version.tar.xz"
    echo "$sha256  $work/linux-$version.tar.xz" | sha256sum -c -
    tar -C "$work" -xJf "$work/linux-$version.tar.xz"
    touch "$source/.sproutfs-unpacked"
fi
cp "$config" "$build/.config"
for option in "${options[@]}"; do "$source/scripts/config" --file "$build/.config" --enable "$option"; done
make -C "$source" O="$build" olddefconfig
# olddefconfig drops an option whose dependencies the configuration lacks, and
# says nothing: each one has to have survived it.
for option in "${options[@]}"; do
    grep -qx "CONFIG_$option=y" "$build/.config" || { echo "CONFIG_$option did not survive olddefconfig" >&2; exit 1; }
done
make -C "$source" O="$build" -j"$(nproc)" vmlinux
echo "linux $(make -s -C "$source" O="$build" kernelrelease) with ${options[*]} built in"
sha256sum "$build/vmlinux"
# Last, so that OUTPUT exists only once the whole build has succeeded.
cp "$build/vmlinux" "$output"
