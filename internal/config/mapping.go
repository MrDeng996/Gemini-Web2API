package config

import (
	"log"
	"os"
	"strings"
	"sync"
)

var (
	modelMapping map[string]string
	mappingMu    sync.RWMutex
	loadOnce     sync.Once
)

// LoadModelMapping 从环境变量 MODEL_MAPPING 加载模型映射。
// 使用 sync.Once 保证只初始化一次，避免并发竞争。
// 若需要热重载，请调用 ReloadModelMapping。
func LoadModelMapping() {
	loadOnce.Do(func() {
		loadModelMappingLocked()
	})
}

// ReloadModelMapping 强制重新加载模型映射（用于 .env 热重载场景）。
func ReloadModelMapping() {
	// 重置 Once，使下次 LoadModelMapping 可重新执行
	loadOnce = sync.Once{}
	loadOnce.Do(func() {
		loadModelMappingLocked()
	})
}

func loadModelMappingLocked() {
	mappingMu.Lock()
	defer mappingMu.Unlock()

	newMapping := make(map[string]string)

	mappingStr := os.Getenv("MODEL_MAPPING")
	if mappingStr == "" {
		modelMapping = newMapping
		return
	}

	pairs := strings.Split(mappingStr, ",")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		parts := strings.SplitN(pair, ":", 2)
		if len(parts) == 2 {
			source := strings.TrimSpace(parts[0])
			target := strings.TrimSpace(parts[1])
			if source != "" && target != "" {
				newMapping[source] = target
				log.Printf("[Config] Model mapping: %s -> %s", source, target)
			}
		}
	}
	modelMapping = newMapping
}

// MapModel 将请求模型名映射到实际 Gemini 模型名。
// 线程安全，首次调用时懒加载映射配置。
func MapModel(model string) string {
	LoadModelMapping()

	mappingMu.RLock()
	defer mappingMu.RUnlock()

	if mapped, ok := modelMapping[model]; ok {
		return mapped
	}
	return model
}
