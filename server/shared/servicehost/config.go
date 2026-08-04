// Package servicehost 提供所有部署单元共用的配置读取与 HTTP 生命周期管理。
package servicehost

import (
	"fmt"
	"os"
	"strings"
)

const devEnvironment = "dev"

// Config 是五个服务共用的基础配置。服务专属配置由各自模块读取。
type Config struct {
	Name          string
	Environment   string
	HTTPAddr      string
	GRPCAddr      string
	AdminAddr     string
	MySQLDSN      string
	RedisAddr     string
	InternalToken string
}

// LoadConfig 按服务名读取独立监听地址，避免旧单进程中 FARM_HTTP_ADDR 的歧义。
func LoadConfig(serviceName, defaultHTTPAddr, defaultAdminAddr string) (Config, error) {
	prefix := "FARM_" + strings.ToUpper(strings.ReplaceAll(serviceName, "-", "_"))
	config := Config{
		Name:          serviceName,
		Environment:   strings.ToLower(getenv("FARM_ENV", devEnvironment)),
		HTTPAddr:      getenv(prefix+"_HTTP_ADDR", defaultHTTPAddr),
		GRPCAddr:      getenv(prefix+"_GRPC_ADDR", ""),
		AdminAddr:     getenv(prefix+"_ADMIN_ADDR", defaultAdminAddr),
		MySQLDSN:      getenv("FARM_MYSQL_DSN", "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"),
		RedisAddr:     getenv("FARM_REDIS_ADDR", "127.0.0.1:6379"),
		InternalToken: strings.TrimSpace(os.Getenv("FARM_INTERNAL_TOKEN")),
	}
	if config.HTTPAddr == "" {
		return Config{}, fmt.Errorf("%s: HTTP address must not be empty", serviceName)
	}
	if config.InternalToken == "" {
		return Config{}, fmt.Errorf("%s: FARM_INTERNAL_TOKEN must not be empty", serviceName)
	}
	return config, nil
}

func Getenv(name, fallback string) string { return getenv(name, fallback) }

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
