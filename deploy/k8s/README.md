# benkz K3s 部署

该目录将 Compose 压测拓扑部署为单节点 K3s 集群：

- Gateway 使用 3 副本 Deployment；Pod UID 是动态实例 ID，Pod IP 通过 Redis 租约发现。
- Farm、Social 各 1 副本；MySQL、业务 Redis、事件 Redis 使用独立 PVC。
- Prometheus、Grafana 和数据库 Exporter 一并部署。
- k6 根据历史峰值 16.6 CPU / 8.9 GiB 配置为 20 CPU 请求、22 CPU 上限、12 GiB 上限，可为 10 万 QPS 留出冗余。

## 部署

当前压测主机使用 cgroup v1，因此固定 K3s `v1.34.10+k3s1`；Kubernetes
1.35 及以后不再支持该 cgroup 版本。主机同时运行多套 MySQL，还要先扩大
Linux AIO 配额：

```bash
install -m 0644 deploy/k8s/91-benkz-k3s.conf /etc/sysctl.d/91-benkz-k3s.conf
sysctl -p /etc/sysctl.d/91-benkz-k3s.conf
curl -sfL https://get.k3s.io | \
  INSTALL_K3S_VERSION='v1.34.10+k3s1' \
  INSTALL_K3S_SKIP_SELINUX_RPM=true \
  INSTALL_K3S_EXEC='server --disable traefik --disable servicelb --write-kubeconfig-mode 644' \
  sh -
```

构建、导入并部署全部服务：

```bash
deploy/k8s/build-images.sh
deploy/k8s/deploy.sh
```

默认使用 Docker 传统构建器，以避开当前主机 BuildKit 在读取上下文时的
卡死问题。BuildKit 修复后可执行 `DOCKER_BUILDKIT=1 deploy/k8s/build-images.sh`。

默认入口：

- Web：`http://<node-ip>:31901`
- Gateway：`http://<node-ip>:31902`
- Prometheus：`http://<node-ip>:30909`
- Grafana：`http://<node-ip>:30300`，默认账号密码为 `admin/admin`

K6 常驻 Pod 默认不发流量，压测时通过 `kubectl exec` 在 Pod 内执行脚本。
历史峰值约为 16.6 CPU / 8.9 GiB，当前 requests 为 20 CPU / 10 GiB，limits
为 22 CPU / 12 GiB。

现有 Compose 数据卷不会自动挂载到 K3s。K3s 使用 `benkz` 命名空间和 local-path PVC，迁移正式数据时应通过数据库备份恢复，而不是让两套服务同时写同一个卷。
当前 Secret 和 Grafana 口令仅适用于开发压测环境，不应原样用于生产。
