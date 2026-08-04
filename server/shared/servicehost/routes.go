package servicehost

import (
	"fmt"
	"path/filepath"

	"farm/server/shared/sharding"
)

// LoadRouteTable 支持从仓库根目录或 server 目录启动服务。
func LoadRouteTable(path string) (*sharding.RouteTable, error) {
	routes, err := sharding.LoadRouteTable(path)
	if err == nil || filepath.IsAbs(path) {
		if err != nil {
			return nil, fmt.Errorf("load route table: %w", err)
		}
		return routes, nil
	}
	for _, prefix := range []string{"..", "../..", "../../.."} {
		if routes, candidateErr := sharding.LoadRouteTable(filepath.Join(prefix, path)); candidateErr == nil {
			return routes, nil
		}
	}
	return nil, fmt.Errorf("load route table %q: %w", path, err)
}
