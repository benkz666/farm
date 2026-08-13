#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <binary> <route-table> <image-ref> <output.tar>" >&2
  exit 2
fi

binary=$1
route_table=$2
image_ref=$3
output=$4

if [ ! -x "$binary" ]; then
  echo "binary is not executable: $binary" >&2
  exit 2
fi
if [ ! -f "$route_table" ]; then
  echo "route table does not exist: $route_table" >&2
  exit 2
fi

work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

rootfs=$work_dir/rootfs
layout=$work_dir/layout
mkdir -p "$rootfs/app/deploy" "$rootfs/etc/ssl/certs" "$rootfs/tmp" "$layout/blobs/sha256"
chmod 1777 "$rootfs/tmp"
install -m 0755 "$binary" "$rootfs/app/service"
install -m 0644 "$route_table" "$rootfs/app/deploy/route-table.local.json"

# 服务是静态二进制；只带运行时会用到的证书和时区数据，不依赖远端 Docker 基础镜像。
install -m 0644 /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem \
  "$rootfs/etc/ssl/certs/ca-certificates.crt"
cp -a /usr/share/zoneinfo "$rootfs/usr-share-zoneinfo"
mkdir -p "$rootfs/usr/share"
mv "$rootfs/usr-share-zoneinfo" "$rootfs/usr/share/zoneinfo"
printf 'root:x:0:0:root:/root:/sbin/nologin\nnobody:x:65534:65534:nobody:/:/sbin/nologin\n' \
  >"$rootfs/etc/passwd"
printf 'root:x:0:\nnobody:x:65534:\n' >"$rootfs/etc/group"

tar --sort=name --mtime='UTC 1970-01-01' --numeric-owner --owner=0 --group=0 \
  -C "$rootfs" -cf "$work_dir/layer.tar" .
layer_diff_id=$(sha256sum "$work_dir/layer.tar" | awk '{print $1}')
gzip -n -c "$work_dir/layer.tar" >"$work_dir/layer.tar.gz"
layer_digest=$(sha256sum "$work_dir/layer.tar.gz" | awk '{print $1}')
layer_size=$(wc -c <"$work_dir/layer.tar.gz" | tr -d ' ')
install -m 0644 "$work_dir/layer.tar.gz" "$layout/blobs/sha256/$layer_digest"

created=$(date -u +'%Y-%m-%dT%H:%M:%SZ')
printf '%s' \
  '{"created":"'"$created"'","architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin","SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt","TZ=Asia/Shanghai"],"Entrypoint":["/app/service"],"WorkingDir":"/app","ExposedPorts":{"9002/tcp":{},"9004/tcp":{},"9100/tcp":{},"9202/tcp":{},"9210/tcp":{},"9302/tcp":{},"9310/tcp":{}}},"rootfs":{"type":"layers","diff_ids":["sha256:'"$layer_diff_id"'"]},"history":[{"created":"'"$created"'","created_by":"farm minimal OCI builder"}]}' \
  >"$work_dir/config.json"
config_digest=$(sha256sum "$work_dir/config.json" | awk '{print $1}')
config_size=$(wc -c <"$work_dir/config.json" | tr -d ' ')
install -m 0644 "$work_dir/config.json" "$layout/blobs/sha256/$config_digest"

printf '%s' \
  '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:'"$config_digest"'","size":'"$config_size"'},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:'"$layer_digest"'","size":'"$layer_size"'}]}' \
  >"$work_dir/manifest.json"
manifest_digest=$(sha256sum "$work_dir/manifest.json" | awk '{print $1}')
manifest_size=$(wc -c <"$work_dir/manifest.json" | tr -d ' ')
install -m 0644 "$work_dir/manifest.json" "$layout/blobs/sha256/$manifest_digest"

printf '%s' '{"imageLayoutVersion":"1.0.0"}' >"$layout/oci-layout"
printf '%s' \
  '{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:'"$manifest_digest"'","size":'"$manifest_size"',"annotations":{"org.opencontainers.image.ref.name":"'"$image_ref"'"}}]}' \
  >"$layout/index.json"

mkdir -p "$(dirname "$output")"
tar --sort=name --mtime='UTC 1970-01-01' --numeric-owner --owner=0 --group=0 \
  -C "$layout" -cf "$output" oci-layout index.json blobs
echo "$image_ref $manifest_digest $output"
