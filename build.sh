#!/bin/bash

# Compile for version
make
if [ $? -ne 0 ]; then
    echo "make error"
    exit 1
fi

frp_version=$(./bin/frps --version)
echo "Build version: $frp_version"

# Cross compiles
make -f ./Makefile.cross-compiles

rm -rf ./release/packages
mkdir -p ./release/packages

os_all="linux windows darwin freebsd"
arch_all="386 amd64 arm arm64 mips64 mips64le mips mipsle riscv64 s390x ppc64 ppc64le sparc sparc64 alpha ia64 hppa sh4 sh4eb microblaze microblazeel m68k m68000 xtensa xtensaeb"

cd ./release

for os in $os_all; do
    for arch in $arch_all; do
        frp_dir_name="${frp_version}_${os}_${arch}"
        frp_path="./packages/${frp_dir_name}"

        if [ "$os" == "windows" ]; then
            if [ ! -f "./frpc_${os}_${arch}.exe" ]; then
                continue
            fi
            if [ ! -f "./frps_${os}_${arch}.exe" ]; then
                continue
            fi
            mkdir -p "${frp_path}"
            mv "./frpc_${os}_${arch}.exe" "${frp_path}/frpc.exe"
            mv "./frps_${os}_${arch}.exe" "${frp_path}/frps.exe"
        else
            if [ ! -f "./frpc_${os}_${arch}" ]; then
                continue
            fi
            if [ ! -f "./frps_${os}_${arch}" ]; then
                continue
            fi
            mkdir -p "${frp_path}"
            mv "./frpc_${os}_${arch}" "${frp_path}/frpc"
            mv "./frps_${os}_${arch}" "${frp_path}/frps"
        fi
        cp ../LICENSE "${frp_path}"
        cp -rf ../conf/* "${frp_path}"

        # Packages
        cd ./packages
        if [ "$os" == "windows" ]; then
            zip -rq "${frp_dir_name}.zip" "${frp_dir_name}"
        else
            tar -zcf "${frp_dir_name}.tar.gz" "${frp_dir_name}"
        fi
        cd ..
        rm -rf "${frp_path}"
    done
done

cd -