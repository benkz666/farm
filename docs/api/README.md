# API 文档

`openapi.yaml` 是对外 HTTP 契约；WebSocket 的 AsyncAPI、前端接口目录和 gRPC HTML
由 `tools/api-docs/main.py` 从前后端常量及 proto 生成。

```bash
make api-docs
make api-docs-check
```

开发环境设置 `FARM_ENABLE_API_DOCS=1` 后访问 `/docs/`。生产环境不注册文档路由。
