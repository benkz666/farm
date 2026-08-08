# Farm Kubernetes 本地压测部署

这组清单面向 kind + Docker Desktop 的本地开发和性能压测。默认拓扑是一个
`farmsvr-0`，1024 个逻辑分片全部路由到 `farm-0`；双副本拓扑再切换到
`farmsvr-1`。清单中的密码和 token 都是开发值，**禁止直接用于生产**，生产环境
应替换为 Secret 管理方案。

## 资源与拓扑

业务容器使用 requests = limits，以获得 Guaranteed QoS：

| 工作负载 | 副本/类型 | 资源 |
| --- | --- | --- |
| gateway | Deployment，默认 1 | 2 CPU / 2 GiB |
| farmsvr | StatefulSet，默认 1 | 2 CPU / 4 GiB |
| socialsvr | Deployment，1 | 1 CPU / 1 GiB |
| MySQL 8.4 | StatefulSet，1 + PVC | 2 CPU / 4 GiB |
| Redis 7 | StatefulSet，1 + PVC | 1 CPU / 2 GiB |

Prometheus 和 Grafana 不在这些基础清单中，由 `kube-prometheus-stack` 提供。
`monitoring-values.local.yaml` 是可选的 kind 本地资源配置。

在 Docker Desktop 上运行双 `farmsvr` 副本测试时，建议给 Docker Desktop VM
至少分配 **14 CPU / 24 GiB**；`kind-cluster.yaml` 中也保留了这个说明。

## 创建 kind 集群

```bash
kind create cluster --config deploy/k8s/kind-cluster.yaml
kubectl cluster-info --context kind-farm
```

集群包含 1 个 control-plane 和 2 个 worker。配置文件预留了宿主机
`9002 -> NodePort 30002` 的端口映射；只有额外 apply `gateway-nodeport.yaml`
后才会启用。常规基线也可以只使用后面的 port-forward。

## 构建并加载本地镜像

构建上下文是仓库根目录，服务镜像均使用 `deploy/Dockerfile.server` 的
`PACKAGE` 参数：

```bash
docker build -f deploy/Dockerfile.server --build-arg PACKAGE=./cmd/gateway -t farm/gateway:local .
docker build -f deploy/Dockerfile.server --build-arg PACKAGE=./cmd/farmsvr -t farm/farmsvr:local .
docker build -f deploy/Dockerfile.server --build-arg PACKAGE=./cmd/socialsvr -t farm/socialsvr:local .
docker build -f deploy/Dockerfile.migrate -t farm/migrate:local .
kind load docker-image farm/gateway:local farm/farmsvr:local farm/socialsvr:local farm/migrate:local --name farm
```

清单使用 `imagePullPolicy: IfNotPresent`，因此镜像加载到 kind 节点后不会再
尝试从远程仓库拉取。

## 应用顺序

### 显式顺序（便于定位问题）

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/config/common-env-secret.yaml
kubectl apply -f deploy/k8s/config/route-table-single.yaml
kubectl apply -f deploy/k8s/config/farm-topology-single.yaml

kubectl apply -f deploy/k8s/mysql.yaml
kubectl apply -f deploy/k8s/redis.yaml
kubectl -n farm wait --for=condition=ready pod/mysql-0 --timeout=180s
kubectl -n farm wait --for=condition=ready pod/redis-0 --timeout=180s

kubectl apply -f deploy/k8s/migrate-job.yaml
kubectl -n farm wait --for=condition=complete job/migrate --timeout=300s

kubectl apply -f deploy/k8s/socialsvr.yaml
kubectl apply -f deploy/k8s/farmsvr.yaml
kubectl apply -f deploy/k8s/gateway.yaml
```

迁移 Job 会在容器内等待 MySQL 就绪，并通过 `deploy/Dockerfile.migrate` 运行
一次迁移。若要重新执行已完成的 Job：

```bash
kubectl -n farm delete job/migrate
kubectl apply -f deploy/k8s/migrate-job.yaml
```

### Kustomize 快捷方式

默认 `kustomization.yaml` 包含单副本基线，但不包含 HPA 和可选 NodePort：

```bash
kubectl apply -k deploy/k8s
```

## 单副本与双副本切换

单副本基线由以下两个 ConfigMap 提供：

- `route-table-single.yaml`：`0-1023 -> farm-0`
- `farm-topology-single.yaml`：`{"farm-0":"farmsvr-0.farmsvr:9210"}`

`farmsvr` 是 StatefulSet，Pod 名称和实例 ID 的映射由启动脚本完成：
`farmsvr-0 -> farm-0`、`farmsvr-1 -> farm-1`。扩展到双副本并切换分片路由：

```bash
kubectl apply -f deploy/k8s/config/route-table-dual.yaml
kubectl apply -f deploy/k8s/config/farm-topology-dual.yaml
kubectl -n farm scale statefulset/farmsvr --replicas=2

# 这里使用 subPath 挂载，ConfigMap 变更不会自动更新文件；必须重启进程读取新路由和目标列表。
kubectl -n farm rollout restart statefulset/farmsvr
kubectl -n farm rollout restart deployment/gateway
kubectl -n farm rollout status statefulset/farmsvr
```

双副本的 farm gRPC 目标是：

```json
{"farm-0":"farmsvr-0.farmsvr:9210","farm-1":"farmsvr-1.farmsvr:9210"}
```

恢复基线时，重新 apply `route-table-single.yaml` 和
`farm-topology-single.yaml`，将 StatefulSet 缩回 1 个副本，再重启
`farmsvr` 和 `gateway`。

服务间 DNS 与 compose 对应关系如下：

- MySQL：`mysql:3306`
- Redis：`redis:6379`
- social gRPC：`socialsvr:9204`
- gateway gRPC 基线：`gateway:9202`，实例 ID 为 `gateway-0`
- farmsvr StatefulSet 使用 headless Service `farmsvr`

## HPA 与多 gateway 注意事项

仅 gateway 提供 HPA，目标 CPU 利用率约 70%，范围为 1-3 个副本：

```bash
kubectl apply -f deploy/k8s/gateway-hpa.yaml
kubectl -n farm get hpa gateway
```

集群需要先提供 `metrics.k8s.io`（例如 metrics-server），否则 HPA 无法获得
CPU 指标。`farmsvr` 不能自动扩缩，因为分片路由要求稳定的 StatefulSet
ordinal 和显式 farm 目标列表。

基础 Deployment 固定使用 `gateway-0 -> gateway:9202`，所以 HPA 清单默认不
随 `kustomization.yaml` 启用。多 gateway 之前必须为每个实例配置唯一
`FARM_GATEWAY_INSTANCE_ID`，并维护指向 `gateway-headless` 的
`FARM_GATEWAY_GRPC_TARGETS`；否则应保持单 gateway，让 farm fanout 继续使用
基线配置。

## Prometheus / Grafana

使用 kube-prometheus-stack 安装监控（可选 values 已包含在仓库中）：

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts && \
helm repo update && \
helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
  --namespace monitoring --create-namespace \
  -f deploy/k8s/monitoring-values.local.yaml
```

不需要本地资源限制时可以去掉最后一行 `-f ...`。Grafana 和 Prometheus 的
Service 由 Helm chart 创建，不要把它们复制到 `farm` namespace。

## 访问与压测

基线推荐使用 port-forward：

```bash
kubectl -n farm port-forward svc/gateway 9002:9002
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

也可以启用 kind 端口映射对应的 NodePort：

```bash
kubectl apply -f deploy/k8s/gateway-nodeport.yaml
```

k6 在集群外的宿主机运行，网关通过上述 port-forward 或 NodePort 访问：

```bash
k6 run path/to/your-test.js
```
