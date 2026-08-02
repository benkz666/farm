package servicehost

import (
	"fmt"
	"path/filepath"

	"farm/server/platform/routing"
)

// LoadRouteTable 支持从仓库根目录或 server 目录启动服务。
func LoadRouteTable(path string) (*routing.RouteTable, error) {
	routes, err := routing.LoadRouteTable(path)
	if err == nil || filepath.IsAbs(path) {
		if err != nil {
			return nil, fmt.Errorf("load route table: %w", err)
		}
		return routes, nil
	}
	for _, prefix := range []string{"..", "../..", "../../.."} {
		if routes, candidateErr := routing.LoadRouteTable(filepath.Join(prefix, path)); candidateErr == nil {
			return routes, nil
		}
	}
	return nil, fmt.Errorf("load route table %q: %w", path, err)
}
