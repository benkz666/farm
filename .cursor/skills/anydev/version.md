下载链接构成规则:
- linux amd64: http://mirrors.tencent.com/repository/generic/codev/any/dev/any-cli-{{version}}
- linux arm64: http://mirrors.tencent.com/repository/generic/codev/any/dev/linux-arm/any-cli-{{version}}
- darwin/windows amd64: http://mirrors.tencent.com/repository/generic/codev/any/dev/{{os}}/any-cli-{{version}}
- darwin/windows arm64: http://mirrors.tencent.com/repository/generic/codev/any/dev/{{os}}-arm/any-cli-{{version}}
下载链接举例:
arm: http://mirrors.tencent.com/repository/generic/codev/any/dev/windows-arm/any-cli-3.2.0
amd: http://mirrors.tencent.com/repository/generic/codev/any/dev/windows/any-cli-3.2.0

cliVersion:https://mirrors.tencent.com/repository/generic/codev/any/dev/version
OS=(
"linux"
"darwin"
"windows"
)
ARCH=(
"amd"
"arm"
)
本地bin: anydev/bin/*
二进制命名规则: any-os
如果是windows,需要以exe结尾 例如 any-windows.exe
未经用户允许,禁止使用该方式进行下载,下载前需要询问