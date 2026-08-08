# CLI 命令请求响应参考

本文档记录了 anydev skill 中使用的所有 CLI 命令的参数说明与真实响应示例。

## 1. any login
**用户登录授权**

执行后 CLI 会输出授权链接，用户完成授权后自动获取并缓存鉴权信息

### 命令
```bash
any login
```

## 2. any dev env template recommend
**获取推荐模板列表**

**参数:**
无参数。

### 命令
```bash
any dev env template recommend -d '{}'
```

### 响应 (节选)
```json
{
  "code": 200,
  "data": {
    "all": [
      {
        "templateUid": "6",
        "templateName": "标准通用模板",
        "imageName": "系统镜像",
        "imageUrl": "mirrors.tencent.com/devcloud/codev-tlinux3:0.1.3",
        "languages": [
          "golang 1.20.4",
          "nodejs 18.12.0"
        ],
        "vsCodePlugins": [
          "Vue.volar",
          "remotessh"
        ],
        "startCommand": "npm install",
        "serverType": "cvm",
        "templateType": "system",
        "isOwner": false,
        "creator": "zilchzhong",
        "version": "v202308090007"
      }
    ]
  }
}
```

## 3. any dev env template query
**查询模板详情**

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `templateUid` | string | **是** | 模板 uid |

### 命令
```bash
any dev env template query -d '{"templateUid":"6"}'
```

### 响应 (节选)
```json
{
  "code": 200,
  "message": "",
  "data": {
    "template": {
      "templateUid": "6",
      "templateName": "标准通用模板",
      "serverType": "cvm",
      "templateType": "system",
      "version": "v202308090007"
    },
    "presetValue": {
      "envConfType": {
        "enable": true,
        "notUpdateEnable": false,
        "items": [
          {
            "name": "标准型 （配置：CPU 16核； 内存 32G；硬盘 100G ）",
            "id": 1,
            "value": ""
          }
        ]
      }
    }
  }
}
```

## 4. any dev git projects
**查询用户代码仓库（支持通过 search 参数搜索）**

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `page` | int32 | **是** | 第几页 |
| `perPage` | int32 | **是** | 每页大小 |
| `search` | string | 否 | 查询关键字 |
| `archived` | bool | 否 | 归档状态，默认不区分归档状态 |
| `orderBy` | string | 否 | 排序字段，可选: `name`、`updated`(默认) |

### 命令
```bash
any dev git projects -d '{"page":1,"perPage":5,"search":"aiResource"}'
```

### 响应
```json
{
  "code": 0,
  "message": "",
  "data": {
    "total": 1,
    "items": [
      {
        "id": 1644361,
        "public": false,
        "name": "aiResource",
        "nameWithNamespace": "clouddev/aiResource",
        "path": "aiResource",
        "pathWithNamespace": "clouddev/aiResource",
        "sshUrlToRepo": "git@git.woa.com:clouddev/aiResource.git",
        "httpUrlToRepo": "http://git.woa.com/clouddev/aiResource.git",
        "httpsUrlToRepo": "https://git.woa.com/clouddev/aiResource.git",
        "webUrl": "http://git.woa.com/clouddev/aiResource",
        "createdAt": "2026-01-27T09:18:26+0000",
        "lastActivityAt": "2026-02-05T08:19:26+0000",
        "limit": false
      }
    ]
  }
}
```

## 5. any dev git branches
**查询仓库分支**

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `gitUrl` | string | **是** | git仓库的 url，如 `https://git.woa.com/CodevPro/codev.git` |
| `search` | string | 否 | 查询关键字 |
| `page` | int32 | 否 | 页数 (默认 1) |
| `perPage` | int32 | 否 | 页面大小 (默认 20) |
| `orderBy` | string | 否 | 排序字段，如 `name`、`updated`(默认) |

### 命令
```bash
any dev git branches -d '{"gitUrl":"https://git.woa.com/clouddev/aiResource.git","page":1,"perPage":1}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": {
    "total": 1,
    "items": [
      {
        "name": "master",
        "protected": true,
        "created_at": "2026-01-27T09:19:02+0000",
        "description": ""
      }
    ]
  }
}
```

## 6. any dev env create
**创建云研发环境**

限制说明：
- 仅支持创建 `serverType=cvm` 的 CPU 模板环境
- `gpu` 与 `vm` 模板即使可查询，也不能通过该命令创建
- 不要让用户填写 `tenantId`
- 不要让用户填写 `envVariables`

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `templateUid` | string | **是** | 环境模板 Uid，且必须是 CPU 模板 |
| `gitUrl` | string | 否 | 代码仓库 Url |
| `branch` | string | 否 | 分支名 |
| `envName` | string | 否 | 环境名称（不指定则使用默认名称） |
| `tenantId` | int32 | 否 | 租户 ID |


### 命令
```bash
# 使用默认名称创建
any dev env create -d '{"templateUid":"6"}'

# 创建时指定环境名称
any dev env create -d '{"templateUid":"6","envName":"我的测试环境"}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": {
    "instanceId": "evnIns-6k6dbhhfz9la",
    "paasEnvId": "xiyouliao-2dvjbrqtoa"
  }
}
```

## 7. any dev env status
**查询环境运行状态**

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `instanceId` | string | **是** | 环境 id 主键（优先使用该字段查询） |
| `envId` | string | 否 | 环境 id 主键（效果同 instanceId，后端暂未优先使用） |
| `paasEnvId` | string | 否 | paas 环境 ID（若 instanceId 为空，则采用此字段查询） |
| `force` | bool | 否 | 销毁环境时使用，为 true 时强制销毁（不受锁定限制） |

### 命令
```bash
any dev env status -d '{"instanceId":"evnIns-6k6dbhhfz9la"}'
```

### 响应 (节选)
```json
{
  "code": 200,
  "message": "",
  "data": {
    "envId": "evnIns-6k6dbhhfz9la",
    "gitUrl": "",
    "status": "running",
    "createdTime": "2026-03-20 09:32:24",
    "paasEnvId": "xiyouliao-2dvjbrqtoa",
    "environmentIp": "21.214.193.144",
    "envDomains": {
      "dynamics": "21.214.193.144.devcloud.woa.com",
      "fix": "xiyouliao-any525-test.devcloud.woa.com"
    },
    "serverType": "cvm",
    "templateInfo": {
      "templateUid": "6",
      "templateName": "标准通用模板-lxcfs",
      "imageName": "系统镜像"
    },
    "envName": "我的开发环境",
    "gitUrls": [],
    "cpu": 16000,
    "memory": 32000,
    "disk": 100,
    "environmentHost": "21.6.64.18"
  }
}
```

## 8. any dev env logs
**查询环境创建日志**

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境主键 ID |
| `offset` | int32 | **是** | 偏移量 |
| `length` | int32 | **是** | 请求长度 |
| `taskUid` | string | 否 | 任务 uid |

### 命令
```bash
any dev env logs -d '{"envId":"evnIns-6k6dbhhfz9la","offset":0,"length":2}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": {
    "total": 27,
    "logMessage": [
      "2026-03-20 09:32:23,INFO,2988a3ee901423299c28ccbfeb29af3c,收到创建请求",
      "2026-03-20 09:32:23,INFO,2988a3ee901423299c28ccbfeb29af3c,tag:codev,调度成功,cluster:cls-jb3dsl4s,namespace:codev-2,node:21.6.64.18,diskType:localplus,baseName:,baseVersion:0,baseHostDir:"
    ]
  }
}
```

## 9. any dev env list
**查询环境列表**

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `search` | string | 否 | （已废弃）查询关键字 |
| `bs` | string | 否 | 根据模板的 bs 筛选环境 |

### 命令
```bash
any dev env list -d '{"search":"","bs":""}'
```

### 响应 (节选)
```json
{
  "code": 200,
  "message": "",
  "data": [
    {
      "id": "evnIns-6k6dbhhfz9la",
      "gitUrl": "",
      "status": "running",
      "createdTime": "2026-03-20 09:32:24",
      "paasEnvId": "xiyouliao-2dvjbrqtoa",
      "serverType": "cvm",
      "templateUid": "6",
      "envName": "我的开发环境",
      "cpu": 16000,
      "memory": 32000,
      "resourceType": 1
    }
  ]
}
```

### 响应字段说明

**`resourceType`** — 环境锁定状态：
| 值 | 说明 |
| :--- | :--- |
| `0` | 未锁定（若连续 7 天无连接，环境将自动休眠） |
| `1` | 已锁定（环境不会被自动休眠） |

## 10. any dev env agent init
**初始化环境 agent**

在远端环境中启动 agent 进程，获取 token 用于后续 exec / upload 操作。若已初始化则复用已有 token。

**参数 (flag):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | **是** | 环境 ID |

### 命令
```bash
any dev env agent init -e evnIns-69rg14n8avx2
```

### 响应
```json
{
  "code": 200,
  "data": {
    "taskId": "exec-xiyouliao-2aaf4f4c0a-6ohomyn6n5mm",
    "token": "6oc28eoe45na",
    "status": "waiting"
  }
}
```

---

## 11. any dev env agent status
**查询 agent 运行状态**

查询远端 agent 进程的运行状态和版本信息。

**参数 (flag):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | **是** | 环境 ID |

### 命令
```bash
any dev env agent status -e evnIns-69rg14n8avx2
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": {
    "agentStatus": "running",
    "version": "0.0.1",
    "source": "agent"
  }
}
```

### 响应字段说明

**`agentStatus`** — agent 进程状态：
| 值 | 说明 |
| :--- | :--- |
| `running` | agent 正常运行，可执行 exec / upload |
| `unreachable` | agent 不可达（进程未启动或已崩溃），此时会额外返回 `initStatus` 和 `taskId` |

**`initStatus`** — 初始化任务状态（仅当 `agentStatus` 为 `unreachable` 时返回）：
| 值 | 说明 |
| :--- | :--- |
| `waiting` | 等待执行 |
| `running` | 正在执行中 |
| `successed` | 执行成功 |
| `failed` | 执行失败 |
| `timeout` | 执行超时 |

**`source`** — 响应数据来源：
| 值 | 说明 |
| :--- | :--- |
| `agent` | 从 agent 直接获取 |
| `initTask` | 从后端初始化任务记录查询 |

---

## 12. any dev env agent exec
**远程执行命令**

通过 WebSocket 连接远端 agent 执行命令。

**参数 (flag):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | **是** | 环境 ID |
| `--timeout` | int | 否 | 命令超时时间（秒），默认 0 表示不限制 |

### 参数传递机制

exec 传入的命令会经过本地 shell 和远端 shell 两层解析。CLI 将 `--` 后面的所有参数用空格拼接为单个字符串，然后发送给远端 agent 执行。由于经过两层 shell 解析，中间的引号、转义等特殊字符无法可靠传递。

**关键限制**：
- **引号会被本地 shell 吃掉**：`bash -c "echo hello"` 在本地 shell 解析后引号丢失，远端实际执行 `bash -c echo hello`，其中 `bash -c` 只取第一个参数 `echo` 作为命令，输出为空
- **重定向 `>`、管道 `|`、后台 `&`**：被本地 shell 或远端 shell 错误解析
- **变量 `$`**：被本地 shell 展开

### 注意事项

> **1. 含 `-` 开头参数必须用 `--` 分隔**
>
> ```bash
> # ✅ 正确：用 -- 分隔后，-la 不会被 cobra 解析
> any dev env agent exec -e evnIns-xxx -- ls -la /data
>
> # ❌ 错误：-la 被 CLI 当作自身 flag，报错 unknown shorthand flag: 'l' in -la
> any dev env agent exec -e evnIns-xxx ls -la /data
> ```

> **2. 复杂命令（含引号/重定向/nohup等）必须使用脚本方式**
>
> ```bash
> # ✅ 正确：先写脚本再执行
> any dev env agent exec -e evnIns-xxx -- bash -c "echo '#!/bin/bash' > /data/workspace/app/start.sh"
> any dev env agent exec -e evnIns-xxx -- bash -c "echo 'nohup /usr/bin/python3 /data/workspace/app/main.py > /data/workspace/app/nohup.out 2>&1 &' >> /data/workspace/app/start.sh"
> any dev env agent exec -e evnIns-xxx -- bash /data/workspace/app/start.sh
> ```

> **3. 远端环境运行时依赖**
>
> 远端环境中运行时依赖可能未安装或不在默认 PATH 中，需先确认是否可用，不可用则先安装：
> - node: 检查 `/opt/codev/nodejs/bin/node` 是否存在，或 `source /etc/profile` 后使用 `node`；若不存在则需安装
> - python3: 检查 `/usr/bin/python3` 是否存在；若不存在则需安装（如 `apt-get update && apt-get install -y python3`）
> - go: 检查 `/usr/local/go/bin/go` 是否存在；若不存在则需安装（如下载官方二进制包解压到 `/usr/local/go`）
> - 其他依赖: 同理，先用 `which <命令>` 确认是否存在，不存在则先安装

### 命令
```bash
# 简单命令
any dev env agent exec -e evnIns-69rg14n8avx2 echo hello world

# 含 - 开头参数
any dev env agent exec -e evnIns-69rg14n8avx2 -- ls -la /data
any dev env agent exec -e evnIns-69rg14n8avx2 -- mkdir -p /data/workspace/app

# 带超时
any dev env agent exec -e evnIns-69rg14n8avx2 --timeout 3 sleep 60

# 含引号的参数（如 URL）
any dev env agent exec -e evnIns-69rg14n8avx2 -- curl -s "http://127.0.0.1:8080/"
```

### ❌ 错误用例
```bash
# ❌ bash -c 的引号被本地 shell 吃掉，远端实际执行 bash -c echo（输出为空）
any dev env agent exec -e evnIns-xxx -- bash -c "echo hello"

# ❌ 重定向 > 被本地 shell 解析，nohup 收到不完整参数，报 "missing operand"
any dev env agent exec -e evnIns-xxx -- nohup /usr/bin/python3 /data/app/main.py > /data/app/nohup.out 2>&1 &

# ❌ bash -c 中包含重定向，引号丢失后命令结构被远端 shell 破坏
any dev env agent exec -e evnIns-xxx -- bash -c "nohup node /data/app/index.js > /data/app/nohup.out 2>&1 &"

# ❌ node 不在远端默认 PATH 中
any dev env agent exec -e evnIns-xxx -- nohup node /data/app/index.js > nohup.out 2>&1 &
# 报错：node: command not found

# ❌ python3 -c 的引号被本地 shell 吃掉，报 syntax error
any dev env agent exec -e evnIns-xxx -- python3 -c "print('hello')"

# ❌ 含 - 开头参数未用 -- 分隔
any dev env agent exec -e evnIns-xxx mkdir -p /data/workspace/app
# 报错：unknown shorthand flag: 'p' in -p
```

### 响应
直接输出命令的 stdout，如：
```
hello world
```

---

## 13. any dev env agent upload
**上传文件到远端环境**

通过 WebSocket 将本地文件上传到远端环境指定路径。支持大文件分片传输（32KB/片）。

**参数 (flag + positional):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | **是** | 环境 ID |
| `<src>` | string | **是** | 本地源文件路径（支持 `~` 展开，不支持目录） |
| `<dest>` | string | **是** | 远端目标**文件**路径（必须包含文件名，不能以 `/` 结尾，不能是已存在的目录） |

> **重要**：`<dest>` 必须是完整的文件路径（如 `/tmp/test.txt`），不能是目录路径（如 `/tmp/`）。若目标目录不存在，agent 会自动创建。若目标文件已存在则覆盖。

### 命令
```bash
any dev env agent upload -e evnIns-69rg14n8avx2 /tmp/upload_test.txt /tmp/upload_test.txt

# ~ 路径自动展开
any dev env agent upload -e evnIns-69rg14n8avx2 ~/test.txt /tmp/test.txt
```

> **`<dest>` 路径规则**：
>
> ```bash
> # ✅ 正确：完整的文件路径
> any dev env agent upload -e evnIns-xxx /tmp/test.txt /tmp/test.txt
>
> # ✅ 正确：目标目录不存在时 agent 会自动创建
> any dev env agent upload -e evnIns-xxx /tmp/test.txt /root/newdir/test.txt
>
> # ❌ 错误：目标路径以 / 结尾（目录路径），报错 "上传目标路径必须包含文件名，不能是目录路径"
> any dev env agent upload -e evnIns-xxx /tmp/test.txt /tmp/
>
> # ❌ 错误：目标路径是已存在的目录（如 /tmp），报错 "上传目标路径必须是文件路径，不能是已存在目录"
> any dev env agent upload -e evnIns-xxx /tmp/test.txt /tmp
>
> # ❌ 错误：源路径是目录，报错 "当前仅支持上传单个文件"
> any dev env agent upload -e evnIns-xxx /tmp/ /data/test.txt
> ```

### 响应
```json
{
  "status": "completed",
  "path": "/tmp/upload_test.txt"
}
```

---

## 14. any dev env ssh
**开启环境 SSH 连接**

> 注：`keyName` 保持空字符串即可；如果不指定特定 IDE 版本，不能传递 `ideVersion`（或传空字符串），否则后端会抛出 `parse int failed` 的报错。

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境 id |
| `keyName` | string | **是** | key 名称（为空时传 `""`） |
| `ideName` | string | **是** | 打开 ide 名称（例如 `VSCode`） |
| `ideVersion` | string | 否 | 选填，打开的 ide 版本 |

### 命令
```bash
any dev env ssh -d '{"envId":"evnIns-6k6dbhhfz9la","keyName":"","ideName":"VSCode"}'
```

### 响应 (节选)
```json
{
  "code": 200,
  "message": "",
  "data": {
    "CodeBuddyCN": "codebuddycn://vscode-remote/codebuddy-remote-ssh+root@xiyouliao-any525-test.devcloud.woa.com:36000/data/workspace/?windowId=_blank",
    "GoLand": "jetbrains-gateway://connect#idePath=/codev/ide/GO/GoLand-2023.3.2&...",
    "VSCode": "vscode://vscode-remote/ssh-remote+root@xiyouliao-any525-test.devcloud.woa.com:36000/data/workspace/?windowId=_blank"
  }
}
```

---

## 15. any dev env info update
**更新环境信息**

更新环境的名称等基本信息。

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境 ID |
| `envName` | string | 否 | 新的环境名称 |

### 命令
```bash
any dev env info update -d '{"envId":"evnIns-6k6dbhhfz9la","envName":"新环境名"}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": {}
}
```

## 16. any dev env adjust range
**查询环境 CPU/内存可调整范围**

查询指定环境的 CPU 和内存可调整范围（最小值、最大值、步长、当前值），以及资源配额剩余信息。

限制说明：
- 仅 `cvm` 和 `gpu` 类型环境支持调整
- 临时 GPU 环境不支持调整
- 热更新任务进行中、迁移任务进行中时不支持调整
- 环境状态仅 `running`、`stopped`、`queuing`、`queue_start` 允许调整

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境主键 ID（长度至少 1） |

### 命令
```bash
any dev env adjust range -d '{"envId":"evnIns-6k6dbhhfz9la"}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": {
    "envId": "evnIns-6k6dbhhfz9la",
    "envName": "我的开发环境",
    "serverType": "cvm",
    "status": "running",
    "supported": true,
    "cpu": {
      "current": 16000,
      "min": 1000,
      "max": 32000,
      "step": 1000
    },
    "memory": {
      "current": 32000,
      "min": 1000,
      "max": 64000,
      "step": 1000
    },
    "cpuQuotaRest": {
      "quota": 48000,
      "lockQuota": 0
    },
    "memoryQuotaRest": {
      "quota": 96000,
      "lockQuota": 0
    },
    "diskQuotaRest": {
      "quota": 500,
      "lockQuota": 0
    },
    "mountStorage": false,
    "isTemporaryGpu": false
  }
}
```

### 响应字段说明

**`cpu` / `memory`** — `CliEnvResourceRange` 资源可调整范围：

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `current` | int32 | 当前值（CPU 单位：毫核，1000=1 核；内存单位：MB，1000=1G） |
| `min` | int32 | 可调整最小值（固定为步长值 1000，即 1 核 / 1G） |
| `max` | int32 | 可调整最大值（根据资源配额动态计算） |
| `step` | int32 | 调整步长（固定 1000，即 1 核 / 1G） |

**`cpuQuotaRest` / `memoryQuotaRest` / `diskQuotaRest`** — `CliEnvQuotaRest` 配额剩余：

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `quota` | int32 | 剩余申请额度 |
| `lockQuota` | int32 | 剩余锁定额度 |

**`isTemporaryGpu`** — 是否为临时 GPU 环境（临时 GPU 不支持调整）。

---

## 17. any dev env adjust exec
**执行环境 CPU/内存调整**

根据指定的目标 CPU 和内存值调整环境配置。调整前建议先通过 `adjust range` 命令获取可调整范围。

限制说明：
- `cvmConfig` 不能为空
- CPU 和内存不能同时等于当前值（未发生变化）
- 目标值必须在 `adjust range` 返回的 min~max 范围内
- 目标值必须按步长（1000）对齐

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境主键 ID |
| `envConfigTypeId` | int32 | 否 | 环境配置类型 ID |
| `cvmConfig` | object | **是** | CVM 目标配置 |
| `cvmConfig.cpu` | int32 | **是** | 目标 CPU 值（单位：毫核，1000=1 核） |
| `cvmConfig.memory` | int32 | **是** | 目标内存值（单位：MB，1000=1G） |

### 命令
```bash
# 升配 16C32G → 32C64G
any dev env adjust exec -d '{"envId":"evnIns-6k6dbhhfz9la","cvmConfig":{"cpu":32000,"memory":64000}}'

# 降配回 16C32G
any dev env adjust exec -d '{"envId":"evnIns-6k6dbhhfz9la","cvmConfig":{"cpu":16000,"memory":32000}}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": ""
}
```

> **注意**: 调整后环境会重启，需等待环境恢复 `running` 状态后才能继续操作。

---

## 18. any dev env disk resize check
**查询环境磁盘扩容可调整范围**

查询指定环境的磁盘扩容可调整范围（当前值、最大值、步长），以及已扩容大小和母机剩余可扩容空间。

限制说明：
- 仅 `cvm` 类型环境支持磁盘扩容
- 仅 `running` 状态支持
- `DiskQuotaResize.Enabled` 必须为 true
- 母机剩余磁盘空间（`DiskRemain`）必须大于 0

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境主键 ID（长度至少 1） |

### 命令
```bash
any dev env disk resize check -d '{"envId":"evnIns-6k6dbhhfz9la"}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": {
    "envId": "evnIns-6k6dbhhfz9la",
    "envName": "我的开发环境",
    "serverType": "cvm",
    "status": "running",
    "supported": true,
    "disk": {
      "current": 100,
      "min": 100,
      "max": 500,
      "step": 10
    },
    "diskQuotaRest": {
      "quota": 500,
      "lockQuota": 0
    },
    "mountStorage": false,
    "resized": 0,
    "diskRemain": 400
  }
}
```

### 响应字段说明

**`disk`** — `CliEnvResourceRange` 磁盘可调整范围：

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `current` | int32 | 当前磁盘大小（单位：G，含已扩容） |
| `min` | int32 | 最小值（等于 current，磁盘不可缩小） |
| `max` | int32 | 最大值（= current + diskRemain） |
| `step` | int32 | 步长（固定 10G） |

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `resized` | int32 | 已扩容大小（G） |
| `diskRemain` | int32 | 母机剩余可扩容大小（G） |

---

## 19. any dev env disk resize exec
**执行环境磁盘扩容**

根据指定的目标磁盘大小执行扩容操作。扩容前建议先通过 `disk resize check` 命令获取可调整范围。

限制说明：
- 目标值必须大于当前值
- 目标值必须按步长（10G）对齐
- 目标值不能超过可扩容上限（max）

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境主键 ID（长度至少 1） |
| `size` | uint32 | **是** | 目标磁盘大小（单位：G，必须大于 0） |

### 命令
```bash
# 扩容到 110G（当前 100G + 10G 步长）
any dev env disk resize exec -d '{"envId":"evnIns-6k6dbhhfz9la","size":110}'

# 扩容到 200G
any dev env disk resize exec -d '{"envId":"evnIns-6k6dbhhfz9la","size":200}'
```

### 响应
```json
{
  "code": 200,
  "message": "",
  "data": ""
}
```

---

## 20. any dev env convert
**锁定环境**

锁定环境可防止环境因长期无连接而自动休眠。部署成功后应检查环境锁定状态，未锁定则尝试锁定。
判断环境是否锁定根据 `any dev env list` 返回值中的 `resourceType` 字段：`1` 为已锁定，`0` 为未锁定。

**参数:**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `envId` | string | **是** | 环境主键 ID |

### 命令
```bash
any dev env convert -d '{"envId":"evnIns-6wg5mvzmdl3o"}'
```

### 响应（成功）
```json
{
  "code": 200,
  "data": "evnIns-6wg5mvzmdl3o"
}
```

### 锁定失败处理

锁定失败时（如配额不足），需提示用户：

> ⚠️ 您的环境锁定配额不足，环境未锁定。若连续 7 天无连接，环境将自动休眠。您可解锁无需锁定的环境以释放锁定配额，或者联系O2000增加锁定配额。

## 21. any dev metrics status
**查询环境资源状态总览**

查询环境全部指标（CPU/内存/系统盘/数据盘/GPU/显存）实时利用率 + 状态判断，环境存在预约休眠时会额外展示休眠信息。资源查询类命令均为只读操作，无需二次确认。

**参数 (flag):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | 否 | 环境 ID，不传则取环境变量 `ANY_ENVID` |

### 命令
```bash
any dev metrics status -e evnIns-72ye3zafco7a
```

### 响应
```
环境资源监控概览 - evnIns-72ye3zafco7a
+--------------+--------+-----------+
|     指标     | 利用率 |   状态    |
+--------------+--------+-----------+
| CPU 利用率   | 0.00%  | [OK] 正常 |
| 数据盘利用率 | 0.49%  | [OK] 正常 |
| 内存利用率   | 0.49%  | [OK] 正常 |
| 系统盘利用率 | 0.22%  | [OK] 正常 |
+--------------+--------+-----------+
```
> 若某指标状态为 `[WARN]`，回复用户时需结合该指标补充一句处理建议（详见 SKILL.md 3.10 流程 A）。GPU/显存在 `cvm` 环境下无数据会合理降级，不报错。

---

## 22. any dev metrics list
**查询环境资源利用率当前值**

查询指定指标（或全部指标）的当前利用率和状态。

**参数 (flag):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | 否 | 环境 ID |
| `-m` / `--metrics` | string | 否 | 指标，逗号分隔：`cpu`/`memory`/`system_disk`/`data_disk`/`gpu`/`vram`；不传则查全部指标 |

### 命令
```bash
# 单指标
any dev metrics list -e evnIns-72ye3zafco7a -m memory

# 多指标
any dev metrics list -e evnIns-72ye3zafco7a -m cpu,memory,system_disk
```

### 响应（单指标）
```
+------------+-----------+
| 内存利用率 |           |
+------------+-----------+
| 利用率     | 0.49%     |
| 状态       | [OK] 正常 |
+------------+-----------+
```

### 响应（多指标，横表）
```
+------------+--------+-----------+
|    指标    | 利用率 |   状态    |
+------------+--------+-----------+
| CPU 利用率 | 0.00%  | [OK] 正常 |
| 内存利用率 | 0.49%  | [OK] 正常 |
+------------+--------+-----------+
```
> `-m` 传入不支持的指标名会报错并提示支持列表。

---

## 23. any dev metrics history
**查询环境资源利用率历史趋势**

按 `--duration` 指定的时间范围查询历史趋势。

**参数 (flag):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | 否 | 环境 ID |
| `-m` / `--metrics` | string | 否 | 指标，逗号分隔；不传则查全部指标 |
| `--duration` | string | 否 | 查询范围，如 `5m`/`30m`/`1h`；不传则使用默认时长（约 5 分钟） |

### 命令
```bash
any dev metrics history -e evnIns-72ye3zafco7a -m cpu --duration 30m
```

### 响应 (节选)
```
CPU 利用率历史趋势
+----------+--------+-------------------------------+
|  时间戳  | 利用率 |            趋势图             |
+----------+--------+-------------------------------+
| 03:08:27 | 0.00%  | ....................          |
| 03:13:27 | 0.00%  | ....................          |
| 03:18:27 | 0.00%  | ....................          |
+----------+--------+-------------------------------+
```
> 数值明显偏高或已接近/超过告警阈值时结合该指标补充处理建议（见 SKILL.md 3.10 流程 D）

---

## 24. any dev metrics alerts
**查询环境高负载告警明细**

查询当前触发中的告警（超阈值指标 + 超出多少 + 已持续多久），环境内直接执行读本地文件，传 `-e` 按 envId 查远端接口。

**参数 (flag):**
| 字段名 | 类型 | 必填 | 说明 |
| :--- | :--- | :--- | :--- |
| `-e` / `--envId` | string | 否 | 环境 ID |
| `-m` / `--metrics` | string | 否 | 按指标过滤，逗号分隔 |

### 命令
```bash
any dev metrics alerts -e evnIns-72ye3zafco7a
```

### 响应
```
+-------------------+--------+--------------+-----------+
|       指标        | 利用率 |  阈值/限制   | 持续时间  |
+-------------------+--------+--------------+-----------+
| [WARN] 内存利用率 | 22.9%  | 告警阈值 85% | 3h6m53s   |
+-------------------+--------+--------------+-----------+
```
> 当前无告警时输出"当前无告警"。结合每条告警指标补充处理建议（见 SKILL.md 3.10 流程 F）；若存在预约休眠，会额外展示 `[SLEEP]` 行，含预计休眠的绝对时间，剩余天数由预计休眠时间与当前时间计算。